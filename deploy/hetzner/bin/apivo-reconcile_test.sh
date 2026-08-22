#!/bin/sh
# Unit-style tests for apivo-reconcile. CI runs this on every PR, for the
# same reason the release workflow proves its own guard before trusting it:
# this script is the only thing standing between a bad image and every
# environment that tracks its channel, and a gate is only as good as the
# proof that it closes.
#
# There is no Docker daemon here, no registry and no container. Every call
# the reconciler makes to the outside goes through $APIVO_DOCKER, so a stub
# stands in for all of it and the tests state the interesting situations
# directly - a registry that will not answer, a rollout whose containers come
# up healthy but on the wrong image, a first-ever deploy that fails and has
# nothing to fall back to.
#
# Everything happens under mktemp. Nothing outside it is read or written.
set -eu

RECONCILE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)/apivo-reconcile

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

STUB_DIR="$TMP/stub"
mkdir -p "$STUB_DIR"

# ---------------------------------------------------------------------------
# The stub docker.
#
# It models just enough of a daemon and a registry to make the reconciler's
# decisions observable: which digest a tag resolves to, whether a rollout
# succeeds, and what the containers claim to be running afterwards. Its
# default behaviour is the happy path, so each test states only its own
# deviation from it.
# ---------------------------------------------------------------------------
cat > "$TMP/docker" <<'STUB'
#!/bin/sh
echo "$*" >> "$STUB_DIR/calls"
read_or() { [ -r "$STUB_DIR/$1" ] && cat "$STUB_DIR/$1" || printf '%s' "$2"; }
case "$1" in
buildx)
    # buildx imagetools inspect <ref> --format <fmt>
    #
    # Faithful to buildx v0.31.1, including its trap: a --format BEGINNING
    # with `{{.Manifest` is ignored and the human-readable block is printed
    # instead. Modelling that is the whole point - the previous stub `cat`ed a
    # bare digest whatever the format, so it proved the reconciler handles a
    # digest correctly and never that it obtains one. The real script shipped
    # unable to resolve a single digest, and this suite stayed green.
    # buildx can also FAIL rather than print an empty digest, and the two are
    # not the same event. The first host ever provisioned hit the failure: the
    # systemd sandbox left $HOME read-only and buildx could not create its
    # state directory. The reconciler discarded that message and reported its
    # generic "cannot resolve", whose suggested causes were all wrong, while a
    # `docker pull` by hand worked perfectly.
    if [ -s "$STUB_DIR/buildx_error" ]; then
        cat "$STUB_DIR/buildx_error" >&2
        exit 1
    fi
    case "$6" in
    '{{.Manifest'*)
        printf 'Name:      %s\nMediaType: application/vnd.oci.image.index.v1+json\nDigest:    sha256:deadbeef\nManifests:\n' "$4"
        ;;
    *)
        case "$4" in
        */api:*) read_or digest_api '' ;;
        */web:*) read_or digest_web '' ;;
        esac
        ;;
    esac
    ;;
pull)
    [ -e "$STUB_DIR/pull_fails" ] && exit 1
    exit 0
    ;;
image)
    case "$2" in
    # The version the release pipeline stamped into the image as an OCI label.
    # api and web are answered separately so a test can present the mismatched
    # pair that a half-moved channel produces.
    inspect)
        case "$5" in
        */web@*) read_or web_label_version "$(read_or label_version 'v0.1.0')" ;;
        *) read_or label_version 'v0.1.0' ;;
        esac
        ;;
    prune) : ;;
    esac
    ;;
container)
    # container inspect --format '{{.Config.Image}}' <name>
    # Answers with whatever the reconciler most recently pinned, which is what
    # a real daemon would say after a successful rollout. A test that wants a
    # container stuck on the old image writes running_override.
    # PER CONTAINER. A single override answered both, so no test could hold
    # exactly one container stale - and with both stale, deleting either of
    # the reconciler's two verify calls still left the suite green. Each check
    # is only genuinely covered when the other one passes.
    case "$5" in
    *-api)
        if [ -e "$STUB_DIR/running_override_api" ]; then
            cat "$STUB_DIR/running_override_api"
        else
            sed -n 's/^APIVO_API_IMAGE=//p' "$STUB_IMAGES_ENV"
        fi
        ;;
    *-web)
        if [ -e "$STUB_DIR/running_override_web" ]; then
            cat "$STUB_DIR/running_override_web"
        else
            sed -n 's/^APIVO_WEB_IMAGE=//p' "$STUB_IMAGES_ENV"
        fi
        ;;
    esac
    ;;
exec)
    # The frontend fetching the api's /healthz. A healthy stack serves the
    # version that was deployed, so that is what the pin says.
    if [ -e "$STUB_DIR/served_override" ]; then
        cat "$STUB_DIR/served_override"
    else
        sed -n 's/^APIVO_VERSION=//p' "$STUB_IMAGES_ENV"
    fi
    ;;
compose)
    case "$2" in
    up)
        # The compose files declare `image: ${APIVO_API_IMAGE:?...}`, so real
        # compose REFUSES to run when the pins are not in the environment.
        # The stub refuses too, and that is the whole point of it: a stub that
        # accepted anything let the quiet path ship calling `up` without ever
        # loading images.env, which broke every sixty-second tick on every
        # host while the suite stayed green.
        if [ -z "${APIVO_API_IMAGE:-}" ] || [ -z "${APIVO_WEB_IMAGE:-}" ]; then
            echo "stub compose: APIVO_API_IMAGE/APIVO_WEB_IMAGE not set" >&2
            exit 1
        fi
        # One exit code per line in compose_exits, consumed in order, so a
        # test can say "the rollout fails and the rollback that follows
        # succeeds". Past the end of the file, everything succeeds.
        n=$(( $(cat "$STUB_DIR/up_count" 2>/dev/null || echo 0) + 1 ))
        echo "$n" > "$STUB_DIR/up_count"
        code=$(sed -n "${n}p" "$STUB_DIR/compose_exits" 2>/dev/null || true)
        exit "${code:-0}"
        ;;
    esac
    ;;
esac
exit 0
STUB
chmod +x "$TMP/docker"

export STUB_DIR
export APIVO_DOCKER="$TMP/docker"
export APIVO_ETC="$TMP/etc"
export APIVO_STATE="$TMP/state"
export STUB_IMAGES_ENV="$APIVO_STATE/qa/images.env"

DIGEST_A="sha256:1111111111111111111111111111111111111111111111111111111111111111"
DIGEST_B="sha256:2222222222222222222222222222222222222222222222222222222222222222"
WEB_A="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
WEB_B="sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
REGISTRY="ghcr.io/nomos-n4s/apivo-news"

FAILS=0

# Reset to the happy path: a fresh QA environment tracking a registry that
# serves DIGEST_A/WEB_A as v0.1.0, and a daemon that does as it is told.
reset() {
    rm -rf "$APIVO_ETC" "$APIVO_STATE" "$STUB_DIR"
    mkdir -p "$APIVO_ETC/qa" "$APIVO_STATE" "$STUB_DIR"
    cat > "$APIVO_ETC/qa/stack.env" <<EOF
APIVO_ENV=qa
APIVO_CHANNEL=qa
APIVO_REGISTRY=$REGISTRY
COMPOSE_FILE=/opt/apivo/compose/docker-compose.yml
EOF
    printf '%s' "$DIGEST_A" > "$STUB_DIR/digest_api"
    printf '%s' "$WEB_A" > "$STUB_DIR/digest_web"
    printf '%s' 'v0.1.0' > "$STUB_DIR/label_version"
}

# Bring the environment to a known-good running state, so a test can then
# break the NEXT rollout and observe the rollback. The call counter is reset
# afterwards, so a test's compose_exits always describes the rollouts it is
# actually about rather than counting from the fixture that set it up.
settle() {
    sh "$RECONCILE" qa >/dev/null 2>&1
    rm -f "$STUB_DIR/up_count"
}

run() {
    # run <env> - captures stdout+stderr and the exit code, never aborts.
    set +e
    OUT=$(sh "$RECONCILE" "$1" 2>&1)
    RC=$?
    set -e
}

check() {
    # check <description> <expected-rc> <required-output-fragment>
    if [ "$RC" -ne "$2" ]; then
        echo "FAIL: $1 - expected exit $2, got $RC: $OUT"
        FAILS=1
        return
    fi
    if [ -n "$3" ] && ! printf '%s' "$OUT" | grep -q -F -e "$3"; then
        echo "FAIL: $1 - exit $2 as expected, but the output never said '$3': $OUT"
        FAILS=1
        return
    fi
    echo "ok: $1"
}

check_state() {
    # check_state <description> <key> <expected>
    got=$(sed -n "s/^$2=//p" "$APIVO_STATE/qa/current" 2>/dev/null | head -n 1)
    if [ "$got" != "$3" ]; then
        echo "FAIL: $1 - current $2 is '${got:-unset}', expected '$3'"
        FAILS=1
    else
        echo "ok: $1"
    fi
}

check_pinned() {
    # check_pinned <description> <expected-api-ref>
    got=$(sed -n 's/^APIVO_API_IMAGE=//p' "$STUB_IMAGES_ENV" 2>/dev/null)
    if [ "$got" != "$2" ]; then
        echo "FAIL: $1 - compose is pinned to '${got:-nothing}', expected '$2'"
        FAILS=1
    else
        echo "ok: $1"
    fi
}

# ===========================================================================
# Configuration refusals. Nothing is attempted, and the exit code says so
# (2, not 1): a misconfigured unit has not failed a deploy, it has failed to
# describe one.
# ===========================================================================

reset
run "../../etc/passwd"
check "an environment name that could escape a path is refused" 2 "not lowercase alphanumeric"

reset
run staging
check "an environment this host does not serve is refused" 2 "does not serve the 'staging' environment"

reset
sed -i 's/^APIVO_CHANNEL=.*//' "$APIVO_ETC/qa/stack.env"
run qa
check "a stack.env missing a required key is refused" 2 "does not set APIVO_CHANNEL"

reset
sed -i 's/^APIVO_ENV=qa/APIVO_ENV=prod/' "$APIVO_ETC/qa/stack.env"
run qa
check "a stack.env whose APIVO_ENV contradicts the unit is refused" 2 "neither is trustworthy"

# The one that matters most of these: a unit pointed at the wrong directory
# would otherwise reconcile production's channel into QA's containers.

# ===========================================================================
# The first deploy.
# ===========================================================================

reset
run qa
check "a fresh environment rolls out" 0 '"event":"rollout_ok"'
check_state "the rollout is recorded" API_DIGEST "$DIGEST_A"
check_state "the version is recorded from the image label" VERSION v0.1.0
check_pinned "compose is pinned by digest, not by tag" "$REGISTRY/api@$DIGEST_A"

# ===========================================================================
# The quiet path - by far the most common tick, and the one that must stay
# silent. A reconciler that logs every minute is a reconciler whose logs
# nobody reads on the day it matters.
# ===========================================================================

reset
settle
run qa
check "an unmoved channel exits quietly" 0 ""
if [ -n "$OUT" ]; then
    echo "FAIL: an unmoved channel said something: $OUT"
    FAILS=1
else
    echo "ok: an unmoved channel says nothing at all"
fi

# ...but it still runs `up`, which is what brings back a container that died
# between ticks. This is the whole self-healing story, so it is asserted
# rather than assumed.
if [ "$(cat "$STUB_DIR/up_count")" != "1" ]; then
    echo "FAIL: an unmoved channel did not converge the stack (up ran $(cat "$STUB_DIR/up_count" 2>/dev/null || echo 0) times, expected 1)"
    FAILS=1
else
    echo "ok: an unmoved channel still converges the running stack"
fi

# ===========================================================================
# A channel that moved.
# ===========================================================================

reset
settle
printf '%s' "$DIGEST_B" > "$STUB_DIR/digest_api"
printf '%s' "$WEB_B" > "$STUB_DIR/digest_web"
printf '%s' 'v0.2.0' > "$STUB_DIR/label_version"
run qa
check "a moved channel rolls forward" 0 '"event":"rollout_ok"'
check_state "the new digest is recorded" API_DIGEST "$DIGEST_B"
check_state "the new version is recorded" VERSION v0.2.0

# ===========================================================================
# A channel caught half-moved.
#
# publish.yml points api:<channel> and web:<channel> at their digests in two
# separate registry calls, and a tick can land between them. Rolling that out
# would put a new api beside the previous frontend and report success, because
# the only version assertion downstream is against the api.
# ===========================================================================

reset
settle
printf '%s' "$DIGEST_B" > "$STUB_DIR/digest_api"
printf '%s' 'v0.2.0' > "$STUB_DIR/label_version"
printf '%s' 'v0.1.0' > "$STUB_DIR/web_label_version"
run qa
check "a half-moved channel is not rolled out" 0 '"event":"version_skew"'
check_state "and the environment stays on the matched pair it had" API_DIGEST "$DIGEST_A"
if [ -e "$STUB_DIR/up_count" ]; then
    echo "FAIL: a half-moved channel still ran compose up"
    FAILS=1
else
    echo "ok: a half-moved channel touches the running stack not at all"
fi

# ...and the very next tick, once the other tag has caught up, deploys it.
printf '%s' "$WEB_B" > "$STUB_DIR/digest_web"
printf '%s' 'v0.2.0' > "$STUB_DIR/web_label_version"
run qa
check "and the tick after the channel settles rolls forward" 0 '"event":"rollout_ok"'
check_state "onto the new pair" API_DIGEST "$DIGEST_B"

# ===========================================================================
# The registry is unreachable.
#
# The single most likely real failure - an expired GHCR credential - and the
# one where doing nothing is the correct action. The environment keeps
# serving.
# ===========================================================================

reset
settle
: > "$STUB_DIR/digest_api"
run qa
check "an unresolvable channel refuses to act" 1 "cannot resolve"
check "and says the environment was left alone" 1 "keeps serving whatever it already had"
check_state "the running state is untouched" API_DIGEST "$DIGEST_A"

# When buildx FAILS rather than returning nothing, its own words must reach
# the operator. This is the regression guard for the first host ever
# provisioned, where the message below was thrown away and the reconcile
# reported three possible causes, none of them the real one.
reset
settle
printf 'ERROR: mkdir /root/.docker/buildx: read-only file system\n' > "$STUB_DIR/buildx_error"
run qa
check "a failing buildx is reported with buildx's own error" 1 "read-only file system"
check "and that failure still says the environment was left alone" 1 "keeps serving whatever it already had"
check "and points at the sandbox, since docker pull by hand would work" 1 "systemd sandbox"
check_state "a failing buildx touches the running state not at all" API_DIGEST "$DIGEST_A"
: > "$STUB_DIR/buildx_error"

# ===========================================================================
# Rollouts that fail.
# ===========================================================================

reset
printf '1\n' > "$STUB_DIR/compose_exits"
run qa
check "a first-ever rollout that fails cannot roll back" 1 '"event":"rollback_impossible"'
check "and says the environment is down" 1 "It is DOWN"
check "and names the pause switch rather than leaving it to be guessed" 1 "paused"

reset
settle
printf '%s' "$DIGEST_B" > "$STUB_DIR/digest_api"
printf '%s' "$WEB_B" > "$STUB_DIR/digest_web"
printf '%s' 'v0.2.0' > "$STUB_DIR/label_version"
# The new rollout fails; the rollback that follows succeeds.
printf '1\n0\n' > "$STUB_DIR/compose_exits"
run qa
check "a failed rollout rolls back" 1 '"event":"rolled_back"'
check_pinned "and compose is pinned to the digest that was serving" "$REGISTRY/api@$DIGEST_A"
check_state "and the recorded state still names the good release" API_DIGEST "$DIGEST_A"
check "and it warns that the channel will be retried" 1 "the next tick will try it again"

reset
settle
printf '1\n1\n' > "$STUB_DIR/compose_exits"
printf '%s' "$DIGEST_B" > "$STUB_DIR/digest_api"
printf '%s' "$WEB_B" > "$STUB_DIR/digest_web"
run qa
check "a failed rollback is reported as down, not as a rollback" 1 '"event":"rollback_failed"'

# ===========================================================================
# Rollouts that LOOK healthy.
#
# `up --wait` returning 0 only says the containers report healthy - and the
# previous container reports healthy just as happily if the new one never
# replaced it. These two tests are the difference between proving a
# roll-forward and assuming one.
# ===========================================================================

# Each container checked independently. With both stale, deleting either
# verify call from the reconciler still left this suite green - so these hold
# exactly ONE container back at a time.
reset
settle
printf '%s' "$DIGEST_B" > "$STUB_DIR/digest_api"
printf '%s' "$WEB_B" > "$STUB_DIR/digest_web"
printf '%s' "$REGISTRY/api@$DIGEST_A" > "$STUB_DIR/running_override_api"
run qa
check "an API container still on the old image fails the rollout" 1 '"event":"digest_mismatch"'
check_pinned "and the stack is put back on the digest that was serving" "$REGISTRY/api@$DIGEST_A"
check_state "and the environment is left on the release that works" API_DIGEST "$DIGEST_A"

reset
settle
printf '%s' "$DIGEST_B" > "$STUB_DIR/digest_api"
printf '%s' "$WEB_B" > "$STUB_DIR/digest_web"
printf '%s' "$REGISTRY/web@$WEB_A" > "$STUB_DIR/running_override_web"
run qa
check "a WEB container still on the old image fails the rollout too" 1 '"event":"digest_mismatch"'
check_state "and that environment is also left on the working release" API_DIGEST "$DIGEST_A"

reset
settle
printf '%s' "$DIGEST_B" > "$STUB_DIR/digest_api"
printf '%s' 'v0.1.0' > "$STUB_DIR/served_override"
printf '%s' 'v0.2.0' > "$STUB_DIR/label_version"
run qa
check "an api serving the wrong version fails the rollout" 1 '"event":"version_mismatch"'

# ===========================================================================
# The maintenance switch.
# ===========================================================================

reset
settle
touch "$APIVO_ETC/qa/paused"
printf '%s' "$DIGEST_B" > "$STUB_DIR/digest_api"
rm -f "$STUB_DIR/calls"
run qa
check "a paused environment does not reconcile" 0 '"event":"paused"'
if [ -e "$STUB_DIR/calls" ]; then
    echo "FAIL: a paused environment still called docker: $(cat "$STUB_DIR/calls")"
    FAILS=1
else
    echo "ok: a paused environment touches nothing at all"
fi

if [ "$FAILS" -ne 0 ]; then
    echo "apivo-reconcile: FAILURES"
    exit 1
fi
echo "apivo-reconcile: all tests passed"
