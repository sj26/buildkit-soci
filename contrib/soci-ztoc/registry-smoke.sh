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
curl --fail --silent --show-error --retry 10 --retry-all-errors --retry-delay 1 --max-time 2 "http://$address/v2/" >/dev/null || {
  status=$?
  docker logs "$registry" >&2
  exit "$status"
}
docker buildx create --name "$builder" --driver docker-container --driver-opt network=host --driver-opt "image=$image" --bootstrap
dd if=/dev/urandom of="$work/first" bs=1M count=12 status=none
dd if=/dev/urandom of="$work/second" bs=1M count=13 status=none
printf 'FROM scratch\nCOPY first /first\nCOPY second /second\n' > "$work/Dockerfile"
# Prime gzip blobs, but no zTOCs, to exercise existing-blob indexing/upload overlap.
docker buildx build --builder "$builder" --provenance=false \
  --output 'type=oci,dest=-,compression=gzip,force-compression=true' "$work" > /dev/null
docker buildx build --builder "$builder" --progress=rawjson --provenance=false \
  --output "type=registry,name=$address/test:ok,registry.insecure=true,oci-mediatypes=true,compression=gzip,force-compression=true,soci=true" \
  "$work" 2>&1 | tee "$work/push.log"

# A retained layer should be ready before the last RUN finishes. The unused
# layer in the COPY source must not be prepared or uploaded speculatively.
mv "$work/Dockerfile" "$work/Dockerfile.original"
dd if=/dev/urandom of="$work/third" bs=1M count=14 status=none
cat > "$work/Dockerfile" <<'DOCKERFILE'
FROM scratch AS source
COPY second /unused
COPY third /copyme
FROM debian:trixie-slim
COPY --link first /first
COPY --link --from=source /copyme /copyme
ARG CHECK=false
RUN sleep 8 && ! $CHECK
DOCKERFILE
docker buildx build --builder "$builder" --progress=rawjson --provenance=false \
  --output "type=registry,name=$address/test:early,registry.insecure=true,oci-mediatypes=true,compression=gzip,force-compression=true,soci=true" \
  "$work" 2>&1 | tee "$work/early.log"
# The ordinary exporter must retain exactly the same image graph.
docker buildx build --builder "$builder" --progress=plain --provenance=false \
  --output "type=registry,name=$address/test:ordinary,registry.insecure=true,oci-mediatypes=true,compression=gzip,force-compression=true" \
  "$work" > "$work/ordinary.log" 2>&1
status=0
timeout --signal=INT 3 docker buildx build --builder "$builder" --provenance=false --build-arg CHECK=cancel \
  --output "type=registry,name=$address/test:canceled,registry.insecure=true,oci-mediatypes=true,compression=gzip,force-compression=true,soci=true" \
  "$work" > "$work/canceled.log" 2>&1 || status=$?
test "$status" = 124
test "$(curl --silent -o /dev/null -w '%{http_code}' "http://$address/v2/test/manifests/canceled")" = 404
if docker buildx build --builder "$builder" --provenance=false --build-arg CHECK=true \
  --output "type=registry,name=$address/test:solve-failed,registry.insecure=true,oci-mediatypes=true,compression=gzip,force-compression=true,soci=true" \
  "$work" > "$work/solve-failed.log" 2>&1; then
  echo 'Failed RUN unexpectedly succeeded' >&2; exit 1
fi
grep -q 'exit code: 1' "$work/solve-failed.log"
test "$(curl --silent -o /dev/null -w '%{http_code}' "http://$address/v2/test/manifests/solve-failed")" = 404
mv "$work/Dockerfile.original" "$work/Dockerfile"

python3 - "$address" "$work/push.log" "$work/early.log" <<'PY'
import hashlib
import json
import sys
import urllib.request

address, log, early_log = sys.argv[1:]
def read(kind, digest):
    request = urllib.request.Request(f'http://{address}/v2/test/{kind}/{digest}', headers={
        'Accept': 'application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json'})
    return urllib.request.urlopen(request).read()

seen = set()
def walk(desc):
    seen.add(desc['digest'])
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
def timestamp(value):
    assert value.endswith('Z'), value
    seconds, _, fraction = value[:-1].partition('.')
    return seconds, fraction.ljust(9, '0')
def interval(status):
    return [timestamp(status[k]) for k in ('started', 'completed')]
overlap = False
for name, status in statuses.items():
    if name.startswith('indexing SOCI layer '):
        indexed = interval(status)
        uploaded = interval(statuses[name.replace('indexing SOCI layer ', 'uploading ready blob ')])
        overlap |= max(indexed[0], uploaded[0]) < min(indexed[1], uploaded[1])
assert overlap, 'No layer upload overlapped its indexing'
print('PASS: timestamps prove upload/index overlap; registry graph is complete and digest-verified')
root = json.loads(read('manifests', 'early'))
seen.clear()
for desc in root['manifests']:
    walk(desc)
image = next(d for d in root['manifests'] if not d.get('artifactType'))
early, ordinary = [json.loads(read('manifests', ref)) for ref in (image['digest'], 'ordinary')]
index = next(d for d in root['manifests'] if d.get('artifactType') == 'application/vnd.amazon.soci.index.v2+json')
assert early.pop('annotations') == {'com.amazon.soci.index-digest': index['digest']}
assert early == ordinary, (early, ordinary)
statuses, vertices = {}, {}
for line in open(early_log):
    value = json.loads(line)
    for status in value.get('statuses', []):
        if 'completed' in status:
            statuses.setdefault(status['id'], status)
    for vertex in value.get('vertexes', []):
        if 'completed' in vertex:
            vertices[vertex['name']] = vertex
last_run = next(v for name, v in vertices.items() if 'RUN sleep 8' in name)
end = timestamp(last_run['completed'])
assert any(name.startswith('streaming SOCI index') and interval(s)[1] < end for name, s in statuses.items())
assert any(name.startswith('uploading ready blob ') and interval(s)[1] < end for name, s in statuses.items())
for name in statuses:
    if name.startswith('uploading ready blob '):
        assert name[len('uploading ready blob '):] in seen, name
print('PASS: index and uploads complete before final RUN; no discarded layers uploaded; ordinary image unchanged')
print('PASS: failed and canceled solves never publish their tags')
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
grep -q 'exit status 73' "$work/failed.log" || { cat "$work/failed.log" >&2; exit 1; }
test "$(curl --silent -o /dev/null -w '%{http_code}' "http://$address/v2/test/manifests/failed")" = 404
echo 'PASS: failed export does not publish a tag'

# A fresh daemon with a broken indexer must reuse the published zTOCs without
# downloading the base image's layers. A new layer must still need the indexer.
docker buildx rm "$builder" >/dev/null
docker buildx create --name "$builder" --driver docker-container --driver-opt network=host --driver-opt "image=$image" --bootstrap
docker exec "buildx_buildkit_${builder}0" sh -c 'printf "#!/bin/sh\nexit 73\n" > /usr/bin/buildkit-soci-ztoc'
printf 'FROM %s/test:ok\n' "$address" > "$work/Dockerfile"
docker logs "$registry" > "$work/registry-before.log" 2>&1
docker buildx build --builder "$builder" --progress=plain --provenance=false \
  --output "type=registry,name=$address/test:reused,registry.insecure=true,oci-mediatypes=true,compression=gzip,force-compression=true,soci=true,soci-cache-from=$address/test:ok" \
  "$work" 2>&1 | tee "$work/reused.log"
grep -q 'reusing remote SOCI ztoc' "$work/reused.log"
docker logs "$registry" > "$work/registry-after.log" 2>&1
python3 - "$address" "$work" <<'PY'
import json
import pathlib
import sys
import urllib.request

address, work = sys.argv[1], pathlib.Path(sys.argv[2])
def manifest(ref):
    request = urllib.request.Request(f'http://{address}/v2/test/manifests/{ref}', headers={
        'Accept': 'application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json'})
    return json.load(urllib.request.urlopen(request))
original, reused = manifest('ok'), manifest('reused')
def soci(root):
    return next(d for d in root['manifests'] if d.get('artifactType') == 'application/vnd.amazon.soci.index.v2+json')
assert soci(original)['digest'] == soci(reused)['digest']
image = manifest(next(d['digest'] for d in original['manifests'] if not d.get('artifactType')))
before, after = [(work / f'registry-{p}.log').read_text() for p in ('before', 'after')]
new_requests = after[len(before):]
for layer in image['layers']:
    assert not any('http.request.method=GET' in line and '/blobs/' + layer['digest'] in line for line in new_requests.splitlines())
print('PASS: fresh builder reused identical zTOCs without downloading image layers')
PY
printf 'COPY first /new\n' >> "$work/Dockerfile"
if docker buildx build --builder "$builder" --provenance=false \
  --output "type=registry,name=$address/test:changed,registry.insecure=true,oci-mediatypes=true,compression=gzip,force-compression=true,soci=true,soci-cache-from=$address/test:ok" \
  "$work" > "$work/changed.log" 2>&1; then
  echo 'New layer incorrectly reused a cached index' >&2; exit 1
fi
grep -q 'exit status 73' "$work/changed.log" || { cat "$work/changed.log" >&2; exit 1; }
test "$(curl --silent -o /dev/null -w '%{http_code}' "http://$address/v2/test/manifests/changed")" = 404
echo 'PASS: a changed layer is not accepted as a remote cache hit'
