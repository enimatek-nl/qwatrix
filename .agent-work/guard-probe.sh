#!/bin/bash
# Prove each federated-join guard has a test that fails without it.
cd /home/enimatek/Projects/qwatrix || exit 1
cp internal/fedapi/joinremote.go /tmp/j.bak
cp internal/roomserver/joinremote.go /tmp/r.bak
restore() { cp /tmp/j.bak internal/fedapi/joinremote.go; cp /tmp/r.bak internal/roomserver/joinremote.go; }

probe() { # name, test, file, from, to
  restore
  NAME="$1"; TEST="$2"; FILE="$3"; FROM="$4"; TO="$5"
  NAME="$NAME" TEST="$TEST" FILE="$FILE" FROM="$FROM" TO="$TO" python3 - <<'PY'
import os
p = os.environ['FILE']
src = open(p).read()
old, new = os.environ['FROM'], os.environ['TO']
if src.count(old) != 1:
    print('PATTERN-MISS', src.count(old)); raise SystemExit(7)
open(p, 'w').write(src.replace(old, new))
PY
  [ $? = 7 ] && { echo "  $NAME: PATTERN DID NOT MATCH"; restore; return; }
  if ! go build ./... >/dev/null 2>&1; then echo "  $NAME: guard does not compile out"; restore; return; fi
  out=$(timeout 200 go test ./tests/api/ -run "$TEST" -count=1 -v 2>&1)
  if echo "$out" | grep -qE '^--- PASS: Test'; then
    if echo "$out" | grep -qE '^--- FAIL: Test'; then echo "  $NAME: fails without the guard, as it should"; else echo "  $NAME: STILL PASSES (test does not bite)"; fi
  else echo "  $NAME: did not run (build error or test name miss)"; fi
  restore
}

probe foreign-room TestAFederatedJoinRefusesStateFromAnotherRoom internal/fedapi/joinremote.go \
  'if declared := event.RoomID().String(); declared != roomID {' 'if declared := event.RoomID().String(); false && declared != roomID {'
probe unsigned TestAFederatedJoinRefusesUnsignedState internal/fedapi/joinremote.go \
  'if err := gomatrixserverlib.VerifyEventSignatures(ctx, event, c.opts.KeyRing, userIDForSender); err != nil {' 'if false {'
probe fake-template TestAFederatedJoinRefusesATemplateForSomebodyElse internal/fedapi/joinremote.go \
  'if err := checkJoinTemplate(proto, userID, roomID); err != nil {
		return nil, err
	}' '_ = checkJoinTemplate(proto, userID, roomID)'
probe contradictory-state TestAFederatedJoinRefusesContradictoryState internal/fedapi/joinremote.go \
  'if seenStateKey[key] {' 'if false {'
probe room-rules TestAFederatedJoinRefusesAPrivateRoom internal/roomserver/joinremote.go \
  'if err := s.events.Authorize(remote.JoinEvent, prior); err != nil {
		return "", err
	}' '_ = s.events.Authorize(remote.JoinEvent, prior)'
probe incomplete-auth-chain TestAFederatedJoinRefusesAPrivateRoom internal/roomserver/joinremote.go \
  'if err := s.requireCompleteAuthChain(remote.JoinEvent, prior); err != nil {
		return "", err
	}' '_ = s.requireCompleteAuthChain(remote.JoinEvent, prior)'
probe answer-without-the-join TestAJoinOfARoomOnAnotherServer internal/roomserver/joinremote.go \
  'if len(prior) == len(state) {' 'if false && len(prior) == len(state) {'
probe wrong-room-answer TestAFederatedJoinRefusesStateFromAnotherRoom internal/roomserver/joinremote.go \
  'if joined := remote.JoinEvent.RoomID().String(); joined != roomID {' 'if joined := remote.JoinEvent.RoomID().String(); false && joined != roomID {'
probe rejoin-shape TestAFederatedJoinOfARejoiningUser internal/fedapi/joinremote.go \
  'if stateKey := event.StateKey(); stateKey != nil && event.EventID() != joinEvent.EventID() {' 'if stateKey := event.StateKey(); stateKey != nil {'

restore
gofmt -w internal/fedapi internal/roomserver >/dev/null
go build ./... && echo "restored and building"
