#!/usr/bin/env bash
set -euo pipefail

baseline=$1
candidate=$2
source_image=$3
output=$4
mkdir -p "$output"
work=$(mktemp -d)
builder="soci-bench-$$"
registry="${builder}-registry"
cleanup_run() {
  docker buildx rm "$builder" >/dev/null 2>&1 || true
  docker rm -fv "$registry" >/dev/null 2>&1 || true
}
cleanup() { cleanup_run; rm -rf "$work"; }
trap cleanup EXIT
context=$work
scenario=existing-gzip
if [[ -d "$source_image" ]]; then
  context=$source_image
  scenario=cold-solve
else
  printf 'FROM %s\n' "$source_image" > "$work/Dockerfile"
fi
docker pull "${REGISTRY_IMAGE:-registry:2}"
docker image inspect "$baseline" >/dev/null 2>&1 || docker pull "$baseline"
docker image inspect "$candidate" >/dev/null 2>&1 || docker pull "$candidate"
lscpu > "$output/host.txt"
docker info >> "$output/host.txt"

# Same host and source; each arm gets a new builder and empty destination registry.
# An image reference primes gzip outside timing to isolate indexing/upload.
# A context directory instead measures cold solve, compression, indexing and push.
for sample in 0 1 2; do
  variants=(baseline candidate)
  (( sample % 2 == 0 )) || variants=(candidate baseline)
  for variant in "${variants[@]}"; do
    image=$baseline
    [[ "$variant" != candidate ]] || image=$candidate
    name="$sample-$variant"
    docker run -d --name "$registry" -p 127.0.0.1::5000 "${REGISTRY_IMAGE:-registry:2}" >/dev/null
    address=$(docker port "$registry" 5000/tcp)
    curl --fail --silent --show-error --retry 10 --retry-all-errors --retry-delay 1 --max-time 2 "http://$address/v2/" >/dev/null || {
      status=$?
      docker logs "$registry" >&2
      exit "$status"
    }
    docker buildx create --name "$builder" --driver docker-container --driver-opt network=host --driver-opt "image=$image" --bootstrap
    if [[ "$scenario" == existing-gzip ]]; then
      docker buildx build --builder "$builder" --progress=plain --provenance=false \
        --output 'type=oci,dest=-,oci-mediatypes=true,compression=gzip,force-compression=true,soci=false' \
        "$context" > /dev/null 2> "$output/$name-prime.log"
    fi
    for phase in cold warm; do
      started=$(date +%s%N)
      docker buildx build --builder "$builder" --progress=plain --provenance=false \
        --build-arg SOURCE_DATE_EPOCH=0 \
        --metadata-file "$output/$name-$phase-metadata.json" \
        --output "type=registry,name=$address/test:$phase,registry.insecure=true,oci-mediatypes=true,compression=gzip,force-compression=true,soci=true" \
        "$context" > "$output/$name-$phase.log" 2>&1
      elapsed=$(( $(date +%s%N) - started ))
      jq -n --arg variant "$variant" --arg phase "$phase" --arg source "$source_image" \
        --arg scenario "$scenario" --arg image "$image" --argjson sample "$sample" --argjson elapsed_ns "$elapsed" \
        '{variant:$variant,phase:$phase,sample:$sample,source:$source,scenario:$scenario,builder:$image,elapsed_s:($elapsed_ns/1000000000)}' \
        | tee "$output/$name-$phase-timing.json"
      curl --fail --silent -H 'Accept: application/vnd.oci.image.index.v1+json' \
        "http://$address/v2/test/manifests/$phase" > "$output/$name-$phase-index.json"
      jq -e '.manifests | any(.artifactType == "application/vnd.amazon.soci.index.v2+json")' "$output/$name-$phase-index.json" > /dev/null
    done
    grep -Eq 'indexing SOCI layer|streaming SOCI index' "$output/$name-cold.log"
    grep -q 'reusing SOCI ztoc' "$output/$name-warm.log"
    if grep -Eq 'indexing SOCI layer|streaming SOCI index' "$output/$name-warm.log"; then
      echo 'Warm export regenerated indexes' >&2; exit 1
    fi
    docker logs "$registry" > "$output/$name-registry.log" 2>&1
    cleanup_run
  done
done

python3 - "$output" <<'PY'
import json
import pathlib
import statistics
import sys

path = pathlib.Path(sys.argv[1])
rows = [json.loads(p.read_text()) for p in path.glob('*-timing.json')]
for sample in range(3):
    # With provenance disabled and identical contents, both exporters must publish
    # exactly the same complete image graph, including the SOCI artifacts.
    roots = [json.loads((path / f'{sample}-{v}-cold-metadata.json').read_text())['containerimage.digest'] for v in ('baseline', 'candidate')]
    assert roots[0] == roots[1], roots
for phase in ('cold', 'warm'):
    values = {v: [r['elapsed_s'] for r in rows if r['variant'] == v and r['phase'] == phase] for v in ('baseline', 'candidate')}
    print(phase, {v: statistics.median(t) for v,t in values.items()})
print('PASS: matched source and registry graphs; fresh-index and reused-index exports')
PY
