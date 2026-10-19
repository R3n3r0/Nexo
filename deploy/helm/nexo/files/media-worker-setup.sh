#!/bin/sh
# Setup of the media worker in Nexo, run by the Job of the Helm hook
# templates/media-worker-setup.yaml after every install and upgrade. It is
# idempotent:
#   1. waits until the Admin API of Nexo answers (SETUP_TIMEOUT seconds);
#   2. creates or updates the least-privilege policy (/setup/policy.json);
#   3. creates the user of the worker, or re-creates it when the secret key
#      of the Secret no longer authenticates (rotated), and attaches the
#      policy;
#   4. points the notify_media_worker target to the Service of the worker,
#      with the shared bearer token.
# nexoctl is copied from the server image by an init container (the images
# of Nexo have no shell). Environment: NEXO_URL, NEXO_ADMIN_URL,
# NEXO_ROOT_USER, NEXO_ROOT_PASSWORD, WORKER_ACCESS_KEY, WORKER_SECRET_KEY,
# WORKER_AUTH_TOKEN, WORKER_ENDPOINT, POLICY_NAME, TARGET, SETUP_TIMEOUT;
# NEXOCTL, POLICY_FILE and NEXOCTL_CONFIG_DIR override the paths of the pod.
# The same script configures the worker of deploy/docker and deploy/swarm,
# which pass the variables of nexo.env and media-worker.env: the root
# credentials may be files (NEXO_ROOT_USER_FILE, NEXO_ROOT_PASSWORD_FILE)
# and the worker ones NEXO_WORKER_ACCESS_KEY, NEXO_WORKER_SECRET_KEY and
# NEXO_WORKER_AUTH_TOKEN.
set -eu

# value VAR: $VAR, else the content of the file named by ${VAR}_FILE.
value() {
    eval "v=\${$1:-} f=\${$1_FILE:-}"
    # shellcheck disable=SC2154 # v and f are set by the eval above
    if [ -z "$v" ] && [ -n "$f" ]; then
        v=$(cat "$f")
    fi
    printf '%s' "$v"
}
NEXO_ROOT_USER=$(value NEXO_ROOT_USER)
NEXO_ROOT_PASSWORD=$(value NEXO_ROOT_PASSWORD)
WORKER_ACCESS_KEY=${WORKER_ACCESS_KEY:-$(value NEXO_WORKER_ACCESS_KEY)}
WORKER_SECRET_KEY=${WORKER_SECRET_KEY:-$(value NEXO_WORKER_SECRET_KEY)}
WORKER_AUTH_TOKEN=${WORKER_AUTH_TOKEN:-$(value NEXO_WORKER_AUTH_TOKEN)}
POLICY_NAME=${POLICY_NAME:-media-worker}
TARGET=${TARGET:-notify_media_worker}
SETUP_TIMEOUT=${SETUP_TIMEOUT:-600}
for var in NEXO_URL NEXO_ADMIN_URL NEXO_ROOT_USER NEXO_ROOT_PASSWORD WORKER_ACCESS_KEY \
    WORKER_SECRET_KEY WORKER_AUTH_TOKEN WORKER_ENDPOINT; do
    eval "[ -n \"\${$var:-}\" ]" || { echo "media-worker-setup: $var is not set" >&2; exit 1; }
done

nexoctl=${NEXOCTL:-/tools/nexoctl}
policy=${POLICY_FILE:-/setup/policy.json}
chmod 0700 "$nexoctl"
NEXOCTL_CONFIG_DIR=${NEXOCTL_CONFIG_DIR:-/tools/config}
NO_COLOR=1
export NEXOCTL_CONFIG_DIR NO_COLOR

log() { echo "media-worker-setup: $*"; }

"$nexoctl" -q alias set nexo "$NEXO_URL" "$NEXO_ROOT_USER" "$NEXO_ROOT_PASSWORD" \
    --no-verify --admin-url "$NEXO_ADMIN_URL"

log "waiting for Nexo at $NEXO_ADMIN_URL"
deadline=$(($(date +%s) + SETUP_TIMEOUT))
until "$nexoctl" -q admin info nexo >/dev/null 2>&1; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
        log "Nexo did not answer within ${SETUP_TIMEOUT}s"
        "$nexoctl" admin info nexo
        exit 1
    fi
    sleep 5
done

"$nexoctl" admin policy create --update nexo "$POLICY_NAME" "$policy"

add_user() {
    "$nexoctl" admin user add nexo "$WORKER_ACCESS_KEY" "$WORKER_SECRET_KEY" --policy "$POLICY_NAME"
}

rc=0
"$nexoctl" -q admin user info nexo "$WORKER_ACCESS_KEY" >/dev/null 2>&1 || rc=$?
case $rc in
0)
    # The user exists: enable it, attach the policy, then check its secret
    # key on the S3 API (exit code 4: wrong secret; a denied listing is
    # accepted).
    "$nexoctl" admin user enable nexo "$WORKER_ACCESS_KEY"
    "$nexoctl" admin policy attach nexo "$POLICY_NAME" --user "$WORKER_ACCESS_KEY"
    rc=0
    "$nexoctl" -q alias set worker "$NEXO_URL" "$WORKER_ACCESS_KEY" "$WORKER_SECRET_KEY" >/dev/null 2>&1 || rc=$?
    case $rc in
    0) ;;
    4)
        log "the secret key of $WORKER_ACCESS_KEY changed: re-creating the user"
        "$nexoctl" admin user remove nexo "$WORKER_ACCESS_KEY"
        add_user
        ;;
    *)
        "$nexoctl" alias set worker "$NEXO_URL" "$WORKER_ACCESS_KEY" "$WORKER_SECRET_KEY"
        exit 1
        ;;
    esac
    ;;
3) add_user ;;
*)
    "$nexoctl" admin user info nexo "$WORKER_ACCESS_KEY"
    exit 1
    ;;
esac

"$nexoctl" admin config set nexo "$TARGET" enable=on "endpoint=$WORKER_ENDPOINT" "auth_token=$WORKER_AUTH_TOKEN"
log "done: $TARGET -> $WORKER_ENDPOINT"
