#!/bin/sh
# usage: cpuprobe.sh <container> <host:port> <scenario substring> [connections]
# 1 リクエストあたりに、入り口のコンテナが使った CPU (cgroup の usage_usec) を測る。負荷をかける側の CPU は含まない
ID=$(docker inspect -f '{{.Id}}' "$1")
F=/sys/fs/cgroup/system.slice/docker-$ID.scope/cpu.stat
[ -f "$F" ] || F=/sys/fs/cgroup/docker/$ID/cpu.stat
cpu() { awk '/^usage_usec/{print $2}' "$F"; }
before=$(cpu)
out=$(docker run --rm --network minecraft-network --cpus 2 --cpu-shares 128 -v /tmp/scenarios.json:/s.json:ro toro-edge:tools \
  node dist/tools/bench.js --target "$2" --scenarios /s.json --seconds 5 --connections "${4:-32}" --only "$3" 2>&1)
after=$(cpu)
total=$(echo "$out" | grep '^RESULT' | sed 's/.*"total":\([0-9]*\).*/\1/')
rps=$(echo "$out" | grep '^RESULT' | sed 's/.*"rps":\([0-9.]*\).*/\1/')
p99=$(echo "$out" | grep '^RESULT' | sed 's/.*"p99":\([0-9.]*\).*/\1/')
echo "$3 | requests=$total rps=$rps p99=${p99}ms | server_cpu_us=$((after-before)) | cpu_us_per_req=$(awk "BEGIN{printf \"%.1f\", ($after-$before)/$total}")"
