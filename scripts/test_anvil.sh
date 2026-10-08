#!/usr/bin/env bash
set -euo pipefail
: "${SETTLEKIT_TEST_DATABASE_URL:?run via scripts/test_postgres.sh --anvil}"
: "${SETTLEKIT_TEST_SERVICE_BINARY:?missing service binary}"
for tool in anvil cast forge jq curl python3 openssl; do
  command -v "$tool" >/dev/null || { echo "missing $tool" >&2; exit 1; }
done

tmp_dir="$(mktemp -d)"
anvil_pid=""
service_pid=""
receiver_pid=""
rpc_proxy_pid=""
cleanup() {
  if [[ -n "$service_pid" ]]; then kill "$service_pid" 2>/dev/null || true; wait "$service_pid" 2>/dev/null || true; fi
  if [[ -n "$receiver_pid" ]]; then kill "$receiver_pid" 2>/dev/null || true; wait "$receiver_pid" 2>/dev/null || true; fi
  if [[ -n "$rpc_proxy_pid" ]]; then kill "$rpc_proxy_pid" 2>/dev/null || true; wait "$rpc_proxy_pid" 2>/dev/null || true; fi
  if [[ -n "$anvil_pid" ]]; then kill "$anvil_pid" 2>/dev/null || true; wait "$anvil_pid" 2>/dev/null || true; fi
  find "$tmp_dir" -depth -delete
}
trap cleanup EXIT

pick_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}
rpc_port="$(pick_port)"
api_port="$(pick_port)"
webhook_port="$(pick_port)"
proxy_port="$(pick_port)"
rpc_url="http://127.0.0.1:$rpc_port"
api_url="http://127.0.0.1:$api_port"
anvil --silent --host 127.0.0.1 --port "$rpc_port" --chain-id 31337 >"$tmp_dir/anvil.log" 2>&1 &
anvil_pid=$!
for _ in {1..50}; do
  if cast rpc eth_chainId --rpc-url "$rpc_url" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
cast rpc eth_chainId --rpc-url "$rpc_url" >/dev/null
accounts="$(cast rpc eth_accounts --rpc-url "$rpc_url")"
admin="$(jq -r '.[0]' <<< "$accounts")"
operator="$(jq -r '.[1]' <<< "$accounts")"
pauser="$(jq -r '.[2]' <<< "$accounts")"
payer="$(jq -r '.[3]' <<< "$accounts")"
payee="$(jq -r '.[4]' <<< "$accounts")"

if ! forge build >"$tmp_dir/forge-build.log" 2>&1; then cat "$tmp_dir/forge-build.log" >&2; exit 1; fi
token="$(forge create --json contracts/test/PaymentEscrow.t.sol:MockUSDC --broadcast --unlocked --from "$admin" --rpc-url "$rpc_url" | sed -n '/^{/,$p' | jq -r .deployedTo)"
escrow="$(forge create --json contracts/src/PaymentEscrow.sol:PaymentEscrow --broadcast --unlocked --from "$admin" --rpc-url "$rpc_url" --constructor-args "$token" "$admin" "$operator" "$pauser" | sed -n '/^{/,$p' | jq -r .deployedTo)"
if [[ -z "$token" || "$token" == "null" || -z "$escrow" || "$escrow" == "null" ]]; then
  echo "local contract deployment failed" >&2; exit 1
fi
cast send "$token" 'mint(address,uint256)' "$payer" 2000000 --unlocked --from "$admin" --rpc-url "$rpc_url" --json >/dev/null
cast send "$token" 'approve(address,uint256)' "$escrow" 2000000 --unlocked --from "$payer" --rpc-url "$rpc_url" --json >/dev/null
python3 scripts/test_rpc_proxy.py "$proxy_port" "$rpc_url" >"$tmp_dir/rpc-proxy.log" 2>&1 &
rpc_proxy_pid=$!
for _ in {1..50}; do
  if cast rpc eth_chainId --rpc-url "http://127.0.0.1:$proxy_port" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
cast rpc eth_chainId --rpc-url "http://127.0.0.1:$proxy_port" >/dev/null

merchant_token="$(openssl rand -hex 32)"
operator_token="$(openssl rand -hex 32)"
webhook_secret="$(openssl rand -hex 32)"
: >"$tmp_dir/webhook-events.jsonl"
start_receiver() {
  SETTLEKIT_TEST_WEBHOOK_SECRET="$webhook_secret" python3 scripts/test_webhook_receiver.py "$webhook_port" "$tmp_dir/webhook-events.jsonl" >"$tmp_dir/receiver.log" 2>&1 &
  receiver_pid=$!
}
start_service() {
  SETTLEKIT_DATABASE_URL="$SETTLEKIT_TEST_DATABASE_URL" \
  SETTLEKIT_RPC_URL="http://127.0.0.1:$proxy_port" \
  SETTLEKIT_RPC_FALLBACK_URL="$rpc_url" \
  SETTLEKIT_CHAIN_ID=31337 \
  SETTLEKIT_ESCROW_ADDRESS="$escrow" \
  SETTLEKIT_TOKEN_ADDRESS="$token" \
  SETTLEKIT_CONFIRMATIONS=2 \
  SETTLEKIT_START_BLOCK=0 \
  SETTLEKIT_LISTEN_ADDRESS="127.0.0.1:$api_port" \
  SETTLEKIT_MERCHANT_API_TOKEN="$merchant_token" \
  SETTLEKIT_OPERATOR_API_TOKEN="$operator_token" \
  SETTLEKIT_WEBHOOK_URL="http://127.0.0.1:$webhook_port" \
  SETTLEKIT_WEBHOOK_SECRET="$webhook_secret" \
  "$SETTLEKIT_TEST_SERVICE_BINARY" >"$tmp_dir/service.log" 2>&1 &
  service_pid=$!
  for _ in {1..100}; do
    if curl -fsS "$api_url/v1/health/ready" >/dev/null 2>&1; then return; fi
    sleep 0.1
  done
  cat "$tmp_dir/service.log" >&2
  return 1
}
start_receiver
start_service

expiry="$(python3 -c 'from datetime import datetime,timedelta,timezone; print((datetime.now(timezone.utc)+timedelta(hours=1)).replace(microsecond=0).isoformat().replace("+00:00","Z"))')"
expiry_unix="$(python3 -c 'from datetime import datetime; import sys; print(int(datetime.fromisoformat(sys.argv[1].replace("Z","+00:00")).timestamp()))' "$expiry")"
body="$(jq -nc --arg payer "$payer" --arg payee "$payee" --arg expires_at "$expiry" '{payer:$payer,payee:$payee,amount_base_units:"1000000",expires_at:$expires_at}')"

wait_status() {
  local intent_id="$1" wanted="$2" got=""
  for _ in {1..100}; do
    got="$(curl -fsS -H "Authorization: Bearer $merchant_token" "$api_url/v1/payment-intents/$intent_id" | jq -r .status)"
    if [[ "$got" == "$wanted" ]]; then return 0; fi
    sleep 0.1
  done
  echo "intent $intent_id status=$got, wanted $wanted" >&2
  cat "$tmp_dir/service.log" >&2
  return 1
}

for action in release refund; do
  key="$(openssl rand -hex 24)"
  intent="$(curl -fsS -X POST "$api_url/v1/payment-intents" -H "Authorization: Bearer $merchant_token" -H "Idempotency-Key: $key" -H 'Content-Type: application/json' -d "$body")"
  intent_id="$(jq -r .id <<< "$intent")"
  expected_id="$(jq -r .escrow_id <<< "$intent")"
  intent_bytes="0x${intent_id//-/}"
  contract_id="$(cast call "$escrow" 'escrowIdFor(bytes16,address,address,uint128,uint64)(bytes32)' "$intent_bytes" "$payer" "$payee" 1000000 "$expiry_unix" --rpc-url "$rpc_url")"
  if [[ "$(tr 'A-F' 'a-f' <<< "$expected_id")" != "$(tr 'A-F' 'a-f' <<< "$contract_id")" ]]; then echo "Go and contract escrow IDs differ" >&2; exit 1; fi
  payer_data="$(jq -r .payer_call.data <<< "$intent")"
  cast send "$escrow" --data "$payer_data" --unlocked --from "$payer" --rpc-url "$rpc_url" --json >/dev/null
  if [[ "$action" == release ]]; then
    wait_status "$intent_id" OBSERVED
    kill "$service_pid"
    wait "$service_pid" 2>/dev/null || true
    service_pid=""
    start_service
  fi
  cast rpc evm_mine --rpc-url "$rpc_url" >/dev/null
  wait_status "$intent_id" CONFIRMED
  if [[ "$action" == release ]]; then
    snapshot="$(cast rpc evm_snapshot --rpc-url "$rpc_url" | tr -d '"')"
  fi
  if [[ "$action" == refund ]]; then
    kill "$receiver_pid"
    wait "$receiver_pid" 2>/dev/null || true
    receiver_pid=""
  fi
  call="$(curl -fsS -X POST "$api_url/v1/payment-intents/$intent_id/$action" -H "Authorization: Bearer $operator_token" -H "Idempotency-Key: $(openssl rand -hex 24)")"
  cast send "$escrow" --data "$(jq -r .data <<< "$call")" --unlocked --from "$operator" --rpc-url "$rpc_url" --json >/dev/null
  cast rpc evm_mine --rpc-url "$rpc_url" >/dev/null
  if [[ "$action" == release ]]; then wait_status "$intent_id" RELEASED; else wait_status "$intent_id" REFUNDED; fi
  if [[ "$action" == release ]]; then
    cast rpc evm_revert "$snapshot" --rpc-url "$rpc_url" >/dev/null
    wait_status "$intent_id" CONFIRMED
    cast send "$escrow" --data "$(jq -r .data <<< "$call")" --unlocked --from "$operator" --rpc-url "$rpc_url" --json >/dev/null
    cast rpc evm_mine --rpc-url "$rpc_url" >/dev/null
    wait_status "$intent_id" RELEASED
  fi
  if [[ "$action" == refund ]]; then
    kill "$service_pid"
    wait "$service_pid" 2>/dev/null || true
    service_pid=""
    start_receiver
    start_service
  fi
done

kill "$rpc_proxy_pid"
wait "$rpc_proxy_pid" 2>/dev/null || true
rpc_proxy_pid=""
kill "$service_pid"
wait "$service_pid" 2>/dev/null || true
service_pid=""
start_service
cast rpc evm_mine --rpc-url "$rpc_url" >/dev/null
target_head="$(cast block-number --rpc-url "$rpc_url")"
checkpoint=""
for _ in {1..100}; do
  checkpoint="$(curl -fsS "$api_url/metrics" | awk '/^settlekit_chain_checkpoint / {print $2}')"
  if [[ "$checkpoint" == "$target_head" ]] && curl -fsS "$api_url/v1/health/ready" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
if [[ "$checkpoint" != "$target_head" ]]; then echo "RPC failover did not reach head $target_head (checkpoint $checkpoint)" >&2; cat "$tmp_dir/service.log" >&2; exit 1; fi

payee_balance="$(cast call "$token" 'balanceOf(address)(uint256)' "$payee" --rpc-url "$rpc_url")"
payer_balance="$(cast call "$token" 'balanceOf(address)(uint256)' "$payer" --rpc-url "$rpc_url")"
if [[ "$payee_balance" != 1000000 && "$payee_balance" != "1000000 [1e6]" ]]; then echo "payee balance $payee_balance" >&2; exit 1; fi
if [[ "$payer_balance" != 1000000 && "$payer_balance" != "1000000 [1e6]" ]]; then echo "payer balance $payer_balance" >&2; exit 1; fi
for _ in {1..100}; do
  released="$(jq -s '[.[] | select(.status == "RELEASED")] | length' "$tmp_dir/webhook-events.jsonl")"
  refunded="$(jq -s '[.[] | select(.status == "REFUNDED")] | length' "$tmp_dir/webhook-events.jsonl")"
  if [[ "$released" -ge 1 && "$refunded" -ge 1 ]]; then break; fi
  sleep 0.1
done
if [[ "$released" -lt 1 || "$refunded" -lt 1 ]]; then echo "signed terminal webhooks not delivered" >&2; cat "$tmp_dir/service.log" >&2; exit 1; fi
reversals="$(jq -s '[.[] | select(.status == "REORGED" and .reverted_event_id != null)] | length' "$tmp_dir/webhook-events.jsonl")"
if [[ "$reversals" -lt 1 ]]; then echo "signed reorg reversal webhook not delivered" >&2; exit 1; fi
echo "Anvil create/fund/confirm/release/refund, reorg reversal, restart recovery, RPC failover, signed webhooks and Go/contract escrow ID: PASS"
