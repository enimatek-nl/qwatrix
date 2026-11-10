#!/bin/sh
# The container entrypoint Complement expects: qwatrix under test, listening on the two
# ports Complement reaches, answering for the server name it was handed.
#
# Complement's image contract is short but specific:
#   - SERVER_NAME arrives as an environment variable and is the identity to serve;
#   - a certificate authority is copied into every container at /complement/ca;
#   - federation traffic is HTTPS on 8448 and Complement verifies nothing (it skips
#     certificate checking but pins the server name), so the leaf has to name the server.
#
# Minting a certificate from an authority that was just handed to us is not a test-only
# shortcut: that is precisely what a deployment behind a private CA does.
set -eu

SERVER_NAME="${SERVER_NAME:-localhost}"
DATA_DIR="${QWATRIX_DATA_DIR:-/data}"
CLIENT_ADDR="${QWATRIX_ADDR:-:8008}"
FEDERATION_ADDR="${QWATRIX_FEDERATION_ADDR:-:8448}"
DB_PATH="${QWATRIX_DB:-$DATA_DIR/qwatrix.db}"
CERT="${QWATRIX_TLS_CERT:-}"
KEY="${QWATRIX_TLS_KEY:-}"

mkdir -p "$DATA_DIR"

# QWATRIX_FEDERATION=false turns the server-server listener off. It is on by default:
# Complement publishes the exposed 8448 for every deployment, so a server that listens on
# nothing answers a peer's first question with a connection reset, which is a worse report
# than "this route is not implemented yet". Set it false to test the client API alone.
FEDERATION_ARGS="-federation-addr $FEDERATION_ADDR"
if [ "${QWATRIX_FEDERATION:-true}" = "false" ]; then
	FEDERATION_ARGS=""
fi

# Complement's fake federation server presents a self-signed certificate of its own making -
# not one signed by the Complement authority it mounts for us - so a homeserver that verifies
# peer certificates strictly can never fetch that peer's signing key, and can therefore never
# believe a signed request from it. This test network is exactly the case the flag exists for.
# Real deployments do not set it, and qwatrix logs a warning when it is on.
export FEDERATION_INSECURE_SKIP_TLS_VERIFY="${QWATRIX_FEDERATION_INSECURE_TLS:-true}"

# The certificate: signed by Complement's authority when there is one, self-signed
# otherwise. A peer will not trust the self-signed case, which is the right outcome — an
# untrusted certificate is a refusal, and federation should refuse rather than pretend.
if [ -z "$CERT" ] || [ -z "$KEY" ]; then
	CERT="$DATA_DIR/$SERVER_NAME.crt"
	KEY="$DATA_DIR/$SERVER_NAME.key"
fi

if [ ! -f "$CERT" ] || [ ! -f "$KEY" ]; then
	if [ -f /complement/ca/ca.crt ] && [ -f /complement/ca/ca.key ]; then
		# subjectAltName, not just the common name: Go's verifier has ignored a bare
		# common name for years, and a certificate that does not name the server is a
		# handshake failure that looks exactly like a federation bug.
		openssl genrsa -out "$KEY" 2048 >/dev/null 2>&1
		openssl req -new -sha256 -key "$KEY" -subj "/CN=$SERVER_NAME" \
			-out "$DATA_DIR/server.csr" >/dev/null 2>&1
		printf 'subjectAltName=DNS:%s\n' "$SERVER_NAME" >"$DATA_DIR/server.ext"
		openssl x509 -req -in "$DATA_DIR/server.csr" \
			-CA /complement/ca/ca.crt -CAkey /complement/ca/ca.key -CAcreateserial \
			-out "$CERT" -days 365 -sha256 -extfile "$DATA_DIR/server.ext" >/dev/null 2>&1
		rm -f "$DATA_DIR/server.csr" "$DATA_DIR/server.ext" "$DATA_DIR/ca.srl"

		# Trust the authority for anything else in this container that verifies
		# certificates, so the server and its neighbours agree about what is trusted.
		cp /complement/ca/ca.crt /usr/local/share/ca-certificates/complement.crt
		update-ca-certificates >/dev/null 2>&1
		echo "minted federation certificate for $SERVER_NAME from the complement authority" >&2
	elif [ ! -f "$CERT" ]; then
		openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 365 \
			-keyout "$KEY" -out "$CERT" -subj "/CN=$SERVER_NAME" \
			-addext "subjectAltName=DNS:$SERVER_NAME" >/dev/null 2>&1
		echo "no certificate authority present; using a self-signed certificate for $SERVER_NAME" >&2
	fi
fi

TLS_ARGS=""
if [ -f "$CERT" ] && [ -f "$KEY" ]; then
	TLS_ARGS="-tls-cert $CERT -tls-key $KEY"
fi

# QWATRIX_ARGS carries whatever the person running the suite wants to add: a different
# default room version, a stricter rate limit, debug logging.
exec /usr/local/bin/qwatrix \
	-server-name "$SERVER_NAME" \
	-addr "$CLIENT_ADDR" \
	-db "$DB_PATH" \
	-data-dir "$DATA_DIR" \
	$FEDERATION_ARGS \
	$TLS_ARGS \
	${QWATRIX_ARGS:-}
