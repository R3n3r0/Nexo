#!/bin/sh
# Generate a development CA and a server certificate for compose.cluster-tls.yml.
#
#   ./gen-dev-certs.sh [extra-hostname ...]
#
# Output (in ./certs, the layout Nexo expects in NEXO_CERTS_DIR):
#   certs/CAs/ca.crt   development CA (trust it in clients: curl --cacert ...)
#   certs/public.crt   server certificate signed by the CA
#   certs/private.key  server private key
#
# The certificate covers the internode names nexo1..nexo4, lb, localhost,
# 127.0.0.1 and every extra hostname given on the command line, so the same
# file set works for the nodes, the load balancer and the healthcheck.
# Never use these certificates in production.
set -eu

out="${CERTS_DIR:-$(dirname "$0")/certs}"
days="${CERT_DAYS:-825}"

if ! command -v openssl >/dev/null 2>&1; then
    echo "openssl is required" >&2
    exit 1
fi

mkdir -p "$out/CAs"
umask 077

if [ ! -f "$out/CAs/ca.key" ]; then
    openssl req -x509 -newkey rsa:3072 -sha256 -nodes -days "$days" \
        -subj "/CN=Nexo development CA" \
        -addext "basicConstraints=critical,CA:TRUE" \
        -addext "keyUsage=critical,keyCertSign,cRLSign" \
        -keyout "$out/CAs/ca.key" -out "$out/CAs/ca.crt"
fi

san="DNS:nexo1,DNS:nexo2,DNS:nexo3,DNS:nexo4,DNS:lb,DNS:localhost,IP:127.0.0.1,IP:::1"
for host in "$@"; do
    san="$san,DNS:$host"
done

openssl req -new -newkey rsa:3072 -sha256 -nodes \
    -subj "/CN=nexo" \
    -keyout "$out/private.key" -out "$out/server.csr"

ext="$out/server.ext"
cat >"$ext" <<EOF
basicConstraints=CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth,clientAuth
subjectAltName=$san
EOF

openssl x509 -req -sha256 -days "$days" \
    -in "$out/server.csr" -CA "$out/CAs/ca.crt" -CAkey "$out/CAs/ca.key" -CAcreateserial \
    -extfile "$ext" -out "$out/public.crt"

rm -f "$out/server.csr" "$ext" "$out/CAs/ca.srl"
# The containers run as UID 65532: the key must be readable by that user.
chmod 0644 "$out/public.crt" "$out/CAs/ca.crt"
chmod 0644 "$out/private.key"

echo "certificates written to $out"
echo "  SANs: $san"
