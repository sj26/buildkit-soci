#!/usr/bin/env bash
set -euo pipefail

image=${1:-buildkit-soci:spike}
work=$(mktemp -d)
builder="soci-smoke-$$"
cleanup() {
  docker buildx rm "$builder" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
docker buildx create --name "$builder" --driver docker-container --driver-opt "image=$image" --bootstrap
dd if=/dev/urandom of="$work/first" bs=1M count=12 status=none
dd if=/dev/urandom of="$work/second" bs=1M count=13 status=none
printf 'FROM scratch\nCOPY first /first\nCOPY second /second\n' > "$work/Dockerfile"

build() {
  docker buildx build --builder "$builder" --progress=plain \
    --output "type=oci,dest=$work/$1.tar,oci-mediatypes=true,compression=gzip,force-compression=true,soci=$2" \
    "$work" 2>&1 | tee "$work/$1.log"
}
build ordinary false
build soci true
build warm true
grep -q 'indexing SOCI layer' "$work/soci.log"
grep -q 'reusing SOCI ztoc' "$work/warm.log"
if grep -q 'indexing SOCI layer' "$work/warm.log"; then
  echo 'Warm build unexpectedly regenerated SOCI indexes' >&2
  exit 1
fi
printf 'changed\n' >> "$work/second"
build changed true
grep -q 'indexing SOCI layer' "$work/changed.log"
grep -q 'reusing SOCI ztoc' "$work/changed.log"

python3 - "$work" <<'PY'
import hashlib
import json
import pathlib
import sys
import tarfile

artifact_type = 'application/vnd.amazon.soci.index.v2+json'

def inspect(name):
    with tarfile.open(pathlib.Path(sys.argv[1]) / f'{name}.tar') as tar:
        def blob(desc):
            data = tar.extractfile('blobs/' + desc['digest'].replace(':', '/')).read()
            assert 'sha256:' + hashlib.sha256(data).hexdigest() == desc['digest']
            assert len(data) == desc['size']
            return json.loads(data)

        root_desc = json.load(tar.extractfile('index.json'))['manifests'][0]
        root = blob(root_desc)
        manifests = root.get('manifests', [])
        if root['mediaType'] == 'application/vnd.oci.image.manifest.v1+json':
            assert name == 'ordinary'
            images, image = [root_desc], root
        else:
            images = [d for d in manifests if d.get('platform', {}).get('os') == 'linux' and not d.get('artifactType')]
            assert len(images) == 1, images
            image = blob(images[0])
        indexes = [d for d in manifests if d.get('artifactType') == artifact_type]
        if name == 'ordinary':
            assert not indexes
            assert 'com.amazon.soci.index-digest' not in image.get('annotations', {})
        else:
            assert len(indexes) == 1
            index_desc = indexes[0]
            assert image['annotations']['com.amazon.soci.index-digest'] == index_desc['digest']
            assert index_desc['annotations']['com.amazon.soci.image-manifest-digest'] == images[0]['digest']
            index = blob(index_desc)
            assert index['config']['mediaType'] == artifact_type
            assert 'subject' not in index
            assert len(index['layers']) == 2
            assert [d['annotations']['com.amazon.soci.image-layer-digest'] for d in index['layers']] == [d['digest'] for d in image['layers']]
            for desc in index['layers']:
                data = tar.extractfile('blobs/' + desc['digest'].replace(':', '/')).read()
                assert 'sha256:' + hashlib.sha256(data).hexdigest() == desc['digest']
        return image['layers'], image['config']

assert inspect('ordinary') == inspect('soci') == inspect('warm')
original, _ = inspect('soci')
changed, _ = inspect('changed')
assert original[0] == changed[0]
assert original[1]['digest'] != changed[1]['digest']
print('PASS: unchanged image layers/config, complete SOCI v2 graph, warm reuse, and changed-layer reindexing')
PY

if docker buildx build --builder "$builder" --output "type=oci,dest=$work/invalid.tar,soci=true,compression=zstd,force-compression=true" "$work" > "$work/invalid.log" 2>&1; then
  echo 'SOCI unexpectedly accepted zstd' >&2
  exit 1
fi
grep -q 'soci=true requires compression=gzip,force-compression=true' "$work/invalid.log"
echo 'PASS: unsupported compression rejected'
