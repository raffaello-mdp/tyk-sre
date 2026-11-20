#!/bin/bash

host=$1 port=$2 max=${3:-60}

if ! exec 3<>"/dev/tcp/$host/$port"; then
  echo "could not open the connection"
  exit 1
fi
echo "connection open"

for ((i = 1; i <= max; i++)); do
  if ! printf 'GET /echo?msg=%d HTTP/1.1\r\nHost: %s\r\n\r\n' "$i" "$host" >&3 2>/dev/null; then
    echo "request $i: connection closed"
    exit 0
  fi

  if ! IFS= read -r -t 3 status <&3; then
    echo "request $i: no response"
    exit 0
  fi
  length=0
  while IFS= read -r -t 3 header <&3 && [[ $header != $'\r' ]]; do
    if [[ ${header,,} == content-length:* ]]; then
      length=${header#*:}
      length=${length//[^0-9]/}
    fi
  done
  ((length > 0)) && read -r -t 3 -N "$length" _ <&3
  echo "request $i: ${status%$'\r'}"
  sleep 1
done
echo "request limit reached"
