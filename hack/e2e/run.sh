#!/bin/sh
# moripa-worlds proxy の docker だけで完結する結合確認。
#   devcontroller (本物の API ハンドラー + メモリ上のワールド) / proxy (このリポジトリのイメージ) /
#   lobby・world (ianua-silvae) を 1 つの docker ネットワークに置き、mcclient.py で
#   ステータス ping・拒否・ロビー経由でのワールドへの移動を確かめる。
#
#   sh hack/e2e/run.sh            (リポジトリのルートで)
#   KEEP=1 sh hack/e2e/run.sh     (終わってもコンテナを残す)
set -eu

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
WORK=${WORK:-$(mktemp -d)}
NET=mwe2e
IANUA=${IANUA_IMAGE:-ghcr.io/morinoparty/ianua-silvae@sha256:9f505aa2dca72b8b85f8a3d91ffab34dc54e8fe877f4a37f0e649014fbe50bec}
IMAGE=${IMAGE:-moripa-worlds:e2e}
GO_IMAGE=${GO_IMAGE:-golang:1.26}
PORT=${PORT:-35565}
SECRET=e2e-forwarding-secret-0123456789
TOKEN=e2e-api-token-0123456789abcdef
SUFFIX=.world.site1.dev.morino.party
W1=0199c8a0-7b3e-7f12-9a4c-1d2e3f405162   # 参加して起動するワールド
W2=0199c8a0-7b3e-7f12-9a4c-000000000002   # 参加しないワールド (ping だけ)
W3=0199c8a0-7b3e-7f12-9a4c-0000000000ff   # schematic の無いワールド
READY_AFTER=${READY_AFTER:-15s}
CLIENT="python3 -I $ROOT/hack/e2e/mcclient.py"

remove() {
	docker rm -f mwe2e-gate mwe2e-ctl mwe2e-lobby mwe2e-world >/dev/null 2>&1 || true
	docker network rm $NET >/dev/null 2>&1 || true
}
cleanup() {
	if [ "${KEEP:-}" != 1 ]; then remove; fi
}
trap cleanup EXIT
remove

echo "== build"
docker build -q -t "$IMAGE" "$ROOT" >/dev/null
mkdir -p "$WORK/bin" "$WORK/schem" "$ROOT/.cache"
docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp -e GOMODCACHE=/cache/mod -e GOCACHE=/cache/build -e CGO_ENABLED=0 \
	-v "$ROOT/.cache:/cache" -v "$ROOT:/src" -v "$WORK/bin:/out" -w /src "$GO_IMAGE" \
	go build -o /out/devcontroller ./hack/devcontroller
python3 -I "$ROOT/hack/e2e/make_schem.py" "$WORK/schem/lobby.schem"
chmod 0755 "$WORK/schem" && chmod 0644 "$WORK/schem/lobby.schem"

echo "== start"
docker network create $NET >/dev/null
for name in lobby world; do
	docker run -d --name mwe2e-$name --network $NET --network-alias $name \
		-e IANUA_VELOCITY_SECRET=$SECRET -e IANUA_SCHEMATIC_PATH=/schem/lobby.schem -e IANUA_MOTD="e2e $name" \
		-v "$WORK/schem:/schem:ro" "$IANUA" >/dev/null
done
docker run -d --name mwe2e-ctl --network $NET --network-alias moripa-worlds \
	-e API_TOKEN=$TOKEN -v "$WORK/bin/devcontroller:/devcontroller:ro" --entrypoint /devcontroller \
	gcr.io/distroless/static-debian12:nonroot -listen :8080 -address world:25565 -ready-after "$READY_AFTER" \
	-world "$W1=テストワールド,$W2=ねむっているワールド" >/dev/null
# 本番と同じく読み取り専用のルート FS・nonroot で動かす (書き込める場所が要らないことも確かめる)
docker run -d --name mwe2e-gate --network $NET -p 127.0.0.1:$PORT:25565 -p 127.0.0.1:38080:8080 \
	--read-only --user 65532:65532 --cap-drop ALL --security-opt no-new-privileges \
	-e FORWARDING_SECRET=$SECRET -e API_TOKEN=$TOKEN -e API_URL=http://moripa-worlds:8080 \
	-e GATE_CONFIG=/etc/gate/config.yml -e STARTUP_WAIT=2m \
	-v "$ROOT/hack/e2e/config.yml:/etc/gate/config.yml:ro" "$IMAGE" proxy >/dev/null

# ロビーとワールドが ping に応答するまで待つ (devcontroller も ready 判定に ping を使う)
for i in $(seq 1 60); do
	if docker logs mwe2e-lobby 2>&1 | grep -qi "listening\|started" && docker logs mwe2e-world 2>&1 | grep -qi "listening\|started"; then break; fi
	sleep 1
done
sleep 2

echo "== status ping"
echo "-- absent world";      $CLIENT status 127.0.0.1 $PORT "$W2$SUFFIX" 777
echo "-- unknown world";     $CLIENT status 127.0.0.1 $PORT "$W3$SUFFIX" 777
echo "-- not a world host";  $CLIENT status 127.0.0.1 $PORT "play.morino.party" 777

echo "== rejected logins"
echo "-- protocol 776 (26.2)"; $CLIENT login 127.0.0.1 $PORT "$W1$SUFFIX" 776 e2e_old 5
echo "-- not a world host";    $CLIENT login 127.0.0.1 $PORT "play.morino.party" 777 e2e_host 5
echo "-- unknown world";       $CLIENT login 127.0.0.1 $PORT "$W3$SUFFIX" 777 e2e_missing 5

echo "== login, wait in lobby, move to world"
$CLIENT login 127.0.0.1 $PORT "$W1$SUFFIX" 777 e2e_player 45 &
CLIENT_PID=$!
sleep 4
echo "-- status while starting"; $CLIENT status 127.0.0.1 $PORT "$W1$SUFFIX" 777
wait $CLIENT_PID
echo "-- status after start";    $CLIENT status 127.0.0.1 $PORT "$W1$SUFFIX" 777

echo "== proxy metrics"
curl -s http://127.0.0.1:38080/metrics | grep -E '^(players_waiting|world_connects_total|logins_total)' || true

echo "== proxy log (selected)"
docker logs mwe2e-gate 2>&1 | grep -E 'denied login|initial server|waiting for world|moved player|connected|switch|registered new server|initializ|plugin|error' | cut -c1-400
echo "== devcontroller log"
docker logs mwe2e-ctl 2>&1 | grep -E 'ensure|request' | cut -c1-200 | tail -n 15
echo "== lobby / world log (player joins)"
docker logs mwe2e-lobby 2>&1 | grep -iE 'e2e_|join|velocity' | tail -n 5
docker logs mwe2e-world 2>&1 | grep -iE 'e2e_|join|velocity' | tail -n 5
