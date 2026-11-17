#!/usr/bin/env bash
# Load test of the audit path: the REAL sender (execute.Chain + execute.HTTPSink, via operator/tools/auditload) -> the reference
# collector (tools/audit-collector, HTTPS + bearer token, private CA) -> MinIO with Object Lock (COMPLIANCE). All on localhost,
# no cluster. For each scenario: what the sender reports as not delivered, what the bucket holds, and whether `rightsize
# audit-verify` on the bucket's copy finds the loss (a gap in the chain) when there is one.
#
#   MINIO=/path/to/minio(.exe) bash hack/lab-audit-load.sh
#
# MinIO is built from source (hack/lab/minio/Dockerfile pins the version): GOBIN=.lab/load go install github.com/minio/minio@<ref>
set -u
HERE="${HERE:-$(cd "$(dirname "$0")/.." && pwd)}"; LAB="$HERE/.lab/load"; mkdir -p "$LAB"
EXE=""; case "$(uname -s)" in MINGW*|MSYS*) SL=//; EXE=.exe;; *) SL=/;; esac
MINIO="${MINIO:-$LAB/minio$EXE}"; [ -x "$MINIO" ] || { echo "no MinIO binary at $MINIO"; exit 2; }
RS="$LAB/rightsize$EXE"; COL="$LAB/audit-collector$EXE"; LOAD="$LAB/auditload$EXE"
(cd "$HERE/operator" && go build -o "$RS" ./cmd/rightsize && go build -o "$LOAD" ./tools/auditload) || exit 2
(cd "$HERE/tools/audit-collector" && go build -o "$COL" .) || exit 2

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "${SL}CN=kubecost-load-ca" \
  -keyout "$LAB/ca.key" -out "$LAB/ca.crt" >/dev/null 2>&1 || exit 2
printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\n' > "$LAB/san.ext"
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "${SL}CN=audit-collector" -keyout "$LAB/tls.key" -out "$LAB/tls.csr" >/dev/null 2>&1
openssl x509 -req -in "$LAB/tls.csr" -CA "$LAB/ca.crt" -CAkey "$LAB/ca.key" -CAcreateserial -days 1 -extfile "$LAB/san.ext" -out "$LAB/tls.crt" >/dev/null 2>&1 || exit 2
printf 'tok-%s' "$(openssl rand -hex 16)" > "$LAB/token"
export S3_ACCESS_KEY="load$(openssl rand -hex 6)" S3_SECRET_KEY="$(openssl rand -hex 20)"
export MINIO_ROOT_USER="$S3_ACCESS_KEY" MINIO_ROOT_PASSWORD="$S3_SECRET_KEY"

stop() { [ -n "${1:-}" ] || return 0; kill "$1" 2>/dev/null; wait "$1" 2>/dev/null; }
rm -rf "$LAB/minio-data"; mkdir -p "$LAB/minio-data"
"$MINIO" server "$LAB/minio-data" --address 127.0.0.1:9310 --console-address 127.0.0.1:9311 > "$LAB/minio.log" 2>&1 &
MPID=$!
trap 'stop "${CPID:-}"; stop "$MPID"' EXIT
for i in $(seq 1 30); do curl -fs -m 2 http://127.0.0.1:9310/minio/health/ready >/dev/null && break; sleep 1; done
curl -fs -m 2 http://127.0.0.1:9310/minio/health/ready >/dev/null || { tail -5 "$LAB/minio.log"; exit 2; }

collector() { # bucket
  "$COL" serve --s3-endpoint 127.0.0.1:9310 --s3-plain-http --bucket "$1" --create-bucket --listen 127.0.0.1:8443 \
    --tls-cert "$LAB/tls.crt" --tls-key "$LAB/tls.key" --token-file "$LAB/token" --retain 2h >> "$LAB/collector-$1.log" 2>&1 &
  CPID=$!
  for i in $(seq 1 20); do python -c "import socket; socket.create_connection(('127.0.0.1', 8443), 1)" 2>/dev/null && return 0; sleep 0.5; done
  echo "collector did not start"; tail -5 "$LAB/collector-$1.log"; exit 2
}
send() { # bucket label events rate
  rm -f "$LAB/$1.local.jsonl"
  "$LOAD" --url https://127.0.0.1:8443/ --ca-file "$LAB/ca.crt" --token-file "$LAB/token" --file "$LAB/$1.local.jsonl" \
    --events "$3" --rate "$4" --label "$2"
}
judge() { # bucket result-line
  "$COL" dump --s3-endpoint 127.0.0.1:9310 --s3-plain-http --bucket "$1" > "$LAB/$1.bucket.jsonl" 2> "$LAB/$1.dump.err"
  stored=$(grep -c . "$LAB/$1.bucket.jsonl"); local_n=$(grep -c . "$LAB/$1.local.jsonl")
  "$RS" audit-verify --audit-file "$LAB/$1.bucket.jsonl" > "$LAB/$1.verify.json" 2>&1; vrc=$?
  python - "$2" "$stored" "$local_n" "$vrc" "$LAB/$1.verify.json" <<'PY'
import json, sys
r = json.loads(sys.argv[1]); stored, local_n, vrc = int(sys.argv[2]), int(sys.argv[3]), int(sys.argv[4])
v = open(sys.argv[5]).read().strip().replace("\n", " ")[:220]
r.update({"localFile": local_n, "inBucket": stored, "sentMinusStored": local_n - stored, "auditVerifyOnBucket": "pass" if vrc == 0 else "BREAK: " + v})
print(json.dumps(r))
PY
}

echo "# audit load test $(date -u +%Y-%m-%dT%H:%M:%SZ): sender queue 1024; each event retried in order with backoff 0.25 s doubling to 5 s, for up to 60 s; 10 s request timeout"
for sc in "steady 600 20" "burst1000 1000 0" "burst3000 3000 0"; do
  set -- $sc; b="load-$1"; collector "$b"; res=$(send "$b" "$1" "$2" "$3"); stop "$CPID"; CPID=""; judge "$b" "$res"
done
# an outage: the collector is stopped for OUT seconds in the middle of a steady 10/s stream
for OUT in 3 15 75; do
  b="load-outage$OUT"; collector "$b"
  rm -f "$LAB/$b.local.jsonl"
  "$LOAD" --url https://127.0.0.1:8443/ --ca-file "$LAB/ca.crt" --token-file "$LAB/token" --file "$LAB/$b.local.jsonl" \
    --events 300 --rate 10 --label "outage${OUT}s" > "$LAB/$b.res" &
  LP=$!; sleep 8; stop "$CPID"; sleep "$OUT"; collector "$b"; wait "$LP"; stop "$CPID"; CPID=""
  judge "$b" "$(cat "$LAB/$b.res")"
done
# the store refuses a delete of a stored event (the property under load is unchanged)
"$COL" probe-delete --s3-endpoint 127.0.0.1:9310 --s3-plain-http --bucket load-burst3000 2>&1 | python -c "import json,sys; d=json.load(sys.stdin); print(json.dumps({'probeDelete': {'deleteRefused': d['deleteRefused'], 'lockMode': d['lockMode']}}))"
