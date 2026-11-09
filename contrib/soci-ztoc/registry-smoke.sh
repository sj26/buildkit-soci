#!/usr/bin/env bash
set -euo pipefail

image=${1:-buildkit-soci:pipelined}
work=$(mktemp -d)
builder="soci-upload-smoke-$$"
registry="${builder}-registry"
cleanup() {
  docker buildx rm "$builder" >/dev/null 2>&1 || true
  docker rm -fv "$registry" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
docker run -d --name "$registry" -p 127.0.0.1::5000 "${REGISTRY_IMAGE:-registry:2}" >/dev/null
address=$(docker port "$registry" 5000/tcp)
curl --fail --silent --retry 10 --retry-connrefused --retry-delay 1 "http://$address/v2/" >/dev/null
docker buildx create --name "$builder" --driver docker-container --driver-opt network=host --driver-opt "image=$image" --bootstrap
dd if=/dev/urandom of="$work/first" bs=1M count=12 status=none
dd if=/dev/urandom of="$work/second" bs=1M count=13 status=none
printf 'FROM scratch\nCOPY first /first\nCOPY second /second\n' > "$work/Dockerfile"
docker buildx build --builder "$builder" --progress=rawjson --provenance=false \
  --output "type=registry,name=$address/test:ok,registry.insecure=true,oci-mediatypes=true,compression=gzip,force-compression=true,soci=true" \
  "$work" 2>&1 | tee "$work/push.log"

python3 - "$address" "$work/push.log" <<'PY'
from datetime import datetime
import hashlib
import json
import sys
import urllib.request

address, log = sys.argv[1:]
def read(kind, digest):
    request = urllib.request.Request(f'http://{address}/v2/test/{kind}/{digest}', headers={
        'Accept': 'application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json'})
    return urllib.request.urlopen(request).read()

def walk(desc):
    manifest = desc['mediaType'].endswith('+json') and 'manifest' in desc['mediaType'] or 'index.v1' in desc['mediaType']
    data = read('manifests' if manifest else 'blobs', desc['digest'])
    assert len(data) == desc['size']
    assert 'sha256:' + hashlib.sha256(data).hexdigest() == desc['digest']
    if manifest:
        value = json.loads(data)
        for child in value.get('manifests', []) + value.get('layers', []) + ([value['config']] if 'config' in value else []):
            walk(child)

root = json.loads(read('manifests', 'ok'))
indexes = [d for d in root['manifests'] if d.get('artifactType') == 'application/vnd.amazon.soci.index.v2+json']
assert len(indexes) == 1
for desc in root['manifests']:
    walk(desc)
statuses = {}
for line in open(log):
    for status in json.loads(line).get('statuses', []):
        if 'completed' in status:
            statuses[status['id']] = status
def interval(status):
    return [datetime.fromisoformat(status[k].replace('Z', '+00:00')) for k in ('started', 'completed')]
overlap = False
for name, status in statuses.items():
    if name.startswith('indexing SOCI layer '):
        indexed = interval(status)
        uploaded = interval(statuses[name.replace('indexing SOCI layer ', 'uploading ready blob ')])
        overlap |= max(indexed[0], uploaded[0]) < min(indexed[1], uploaded[1])
assert overlap, 'No layer upload overlapped its indexing'
print('PASS: timestamps prove upload/index overlap; registry graph is complete and digest-verified')
PY

# A failed index must never publish a tag, even if some blobs uploaded already.
docker exec "buildx_buildkit_${builder}0" sh -c 'printf "#!/bin/sh\nexit 73\n" > /usr/bin/buildkit-soci-ztoc'
printf 'changed\n' >> "$work/first"
if docker buildx build --builder "$builder" --provenance=false \
  --output "type=registry,name=$address/test:failed,registry.insecure=true,oci-mediatypes=true,compression=gzip,force-compression=true,soci=true" \
  "$work" > "$work/failed.log" 2>&1; then
  echo 'Failed indexer unexpectedly succeeded' >&2
  exit 1
fi
grep -q 'exit status 73' "$work/failed.log"
test "$(curl --silent -o /dev/null -w '%{http_code}' "http://$address/v2/test/manifests/failed")" = 404
echo 'PASS: failed export does not publish a tag'
