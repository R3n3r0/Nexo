#!/bin/sh
# Generate a development CA and the certificates of the two edge proxies of
# compose.multi-proxy.yml.
#
#   ./multi-proxy/gen-certs.sh [domain-a] [domain-b]     # default alpha.example beta.example
#
# Output (in ../certs, ignored by git):
#   certs/edge-ca.crt              the development CA: trust it in the clients
#                                  (curl --cacert certs/edge-ca.crt ...)
#   certs/proxy-a/fullchain.pem    nginx: <domain-a> and *.<domain-a>
#   certs/proxy-a/privkey.pem
#   certs/proxy-b/beta.pem         HAProxy: certificate and key of <domain-b>
#                                  and *.<domain-b> in one file
# Never use these certificates in production: give each proxy the real
# certificates of its domains (or let Caddy/Traefik obtain them).
set -eu

dir="$(cd "$(dirname "$0")/.." && pwd)"
out="${CERTS_DIR:-$dir/certs}"
days="${CERT_DAYS:-825}"
domain_a="${1:-alpha.example}"
domain_b="${2:-beta.example}"

if ! command -v openssl >/dev/null 2>&1; then
    echo "openssl is required" >&2
    exit 1
fi

mkdir -p "$out/edge-ca" "$out/proxy-a" "$out/proxy-b"
umask 077

if [ ! -f "$out/edge-ca/ca.key" ]; then
    openssl req -x509 -newkey rsa:3072 -sha256 -nodes -days "$days" \
        -subj "/CN=Nexo edge development CA" \
        -addext "basicConstraints=critical,CA:TRUE" \
        -addext "keyUsage=critical,keyCertSign,cRLSign" \
        -keyout "$out/edge-ca/ca.key" -out "$out/edge-ca/ca.crt" 2>/dev/null
fi
cp "$out/edge-ca/ca.crt" "$out/edge-ca.crt"

# leaf <domain> <cert file> <key file>: a certificate for <domain> and
# *.<domain> signed by the development CA.
leaf() {
    ext="$out/leaf.ext"
    cat >"$ext" <<EOF
basicConstraints=CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=DNS:$1,DNS:*.$1
EOF
    openssl req -new -newkey rsa:2048 -sha256 -nodes -subj "/CN=$1" \
        -keyout "$3" -out "$out/leaf.csr" 2>/dev/null
    openssl x509 -req -sha256 -days "$days" -in "$out/leaf.csr" \
        -CA "$out/edge-ca/ca.crt" -CAkey "$out/edge-ca/ca.key" -CAcreateserial \
        -extfile "$ext" -out "$2" 2>/dev/null
    rm -f "$out/leaf.csr" "$ext" "$out/edge-ca/ca.srl"
}

leaf "$domain_a" "$out/proxy-a/fullchain.pem" "$out/proxy-a/privkey.pem"
leaf "$domain_b" "$out/proxy-b/cert.pem" "$out/proxy-b/key.pem"
cat "$out/proxy-b/cert.pem" "$out/proxy-b/key.pem" >"$out/proxy-b/beta.pem"
rm -f "$out/proxy-b/cert.pem" "$out/proxy-b/key.pem"

# The proxies run as other users than the owner of the files: development
# certificates only, readable by everyone.
chmod 0755 "$out/proxy-a" "$out/proxy-b"
chmod 0644 "$out/edge-ca.crt" "$out/proxy-a/fullchain.pem" "$out/proxy-a/privkey.pem" "$out/proxy-b/beta.pem"
echo "certificates of $domain_a (proxy-a) and $domain_b (proxy-b) in $out"
