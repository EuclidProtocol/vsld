#!/bin/bash
set -o errexit -o nounset -o pipefail

BASE_ACCOUNT=$(vsld keys show validator -a --keyring-backend=test)
vsld q auth account "$BASE_ACCOUNT" -o json | jq

echo "## Add new account"
vsld keys add fred --keyring-backend=test

echo "## Check balance"
NEW_ACCOUNT=$(vsld keys show fred -a --keyring-backend=test)
vsld q bank balances "$NEW_ACCOUNT" -o json || true

echo "## Transfer tokens"
vsld tx bank send validator "$NEW_ACCOUNT" 1ustake --gas 1000000 -y --chain-id=testing --node=http://localhost:26657 -b sync -o json --keyring-backend=test | jq

echo "## Check balance again"
vsld q bank balances "$NEW_ACCOUNT" -o json | jq
