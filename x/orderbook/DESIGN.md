# x/orderbook Module Design

## Overview

A native Cosmos SDK module for Euclid's VSL daemon that replaces the current escrow contract + merkle proof withdrawal system with a **lien-based balance model**. Balances are maintained by an existing CosmWasm balance contract, and the module interacts with it via `SudoMsg` to lock, unlock, and transfer tokens. This eliminates the need for explicit deposits while maintaining settlement safety.

The module provides a **dual-lane order system**: a slow lane for on-chain orders (atomic with lien changes, CosmWasm-composable) and a fast lane for offchain orders (sub-second matching via the existing engine within confirmed allowances).

## Problem Statement

The current architecture requires users to maintain two separate balances:

1. **Contract balance** — tokens held in a CosmWasm balance/escrow contract
2. **Orderbook balance** — a separate ledger tracked by the offchain account-service

Moving between them requires:
- **Deposit**: on-chain tx to contract → deposit-listener → account-service → Kafka → engine cache
- **Withdrawal**: engine → account-service → merkle proof generation → permit signing → on-chain tx → withdrawal-watcher → confirmation

This creates UX friction (users manage two balances), composability barriers (other contracts cannot atomically interact with the orderbook), and trust assumptions (operator posts merkle roots).

## Design Goals

1. **Eliminate the two-balance mental model** — a single contract balance, partially liened for trading
2. **Atomic first-trade setup** — set allowance + place order in a single transaction
3. **Preserve offchain matching speed** — sub-second fills for orders within confirmed allowances
4. **Enable CosmWasm composability** — contracts can set allowances and place orders via custom messages
5. **Remove operator trust for withdrawals** — no merkle proofs, no permits, no operator key
6. **Maintain settlement safety** — no fills that can't settle

## Architecture

```
                    ┌─────────────────────┐
                    │  Balance Contract    │
                    │  (CosmWasm)          │
                    │                     │
                    │  - User balances    │
                    │  - Lock/unlock      │◄─── SudoMsg from module
                    │  - Transfer         │◄─── SudoMsg from module
                    │  - Deposit/withdraw │◄─── User ExecuteMsg
                    └────────┬────────────┘
                             │
                    ┌────────▼────────┐
                    │  x/orderbook    │
                    │  module         │
                    │                 │
                    │  - Lien ledger  │◄─── MsgSetTradingAllowance
                    │  - Order gate   │◄─── MsgPlaceOrder (slow lane)
                    │  - Settlement   │◄─── Batched trade results from engine
                    │  - CosmWasm API │◄─── Contract custom messages
                    └────────┬────────┘
                             │ events / gRPC
                    ┌────────▼────────┐
                    │  Offchain       │
                    │  Engine         │◄─── Direct API (fast lane)
                    │                 │
                    │  - Matching     │
                    │  - Order book   │
                    │  - WAL          │
                    └─────────────────┘
```

## Core Concepts

### Balance Contract

Euclid uses a CosmWasm contract as the authoritative balance ledger rather than `x/bank`. The contract tracks per-user balances and supports a `SudoMsg` entry point that only native Cosmos SDK modules can invoke (via `wasmKeeper.Sudo()`). Users cannot call Sudo — it is a privileged interface.

The balance contract must implement the following `SudoMsg` variants for the orderbook module to function:

```rust
#[cw_serde]
pub enum SudoMsg {
    /// Lock tokens in a user's balance, preventing withdrawal or transfer.
    /// Called when a trading allowance is set or increased.
    Lock {
        user: String,
        denom: String,
        amount: Uint128,
    },

    /// Unlock previously locked tokens, making them available again.
    /// Called when a trading allowance is reduced or orders are cancelled.
    Unlock {
        user: String,
        denom: String,
        amount: Uint128,
    },

    /// Transfer tokens between two users within the contract.
    /// Called during trade settlement.
    Transfer {
        from: String,
        to: String,
        denom: String,
        amount: Uint128,
    },

    /// Query a user's balance (available and locked).
    /// Returns { available: Uint128, locked: Uint128 }.
    QueryBalance {
        user: String,
        denom: String,
    },
}
```

The module calls these via the existing wasm keeper:

```go
func (k Keeper) lockTokens(ctx sdk.Context, user sdk.AccAddress, denom string, amount math.Int) error {
    msg := fmt.Sprintf(
        `{"lock":{"user":"%s","denom":"%s","amount":"%s"}}`,
        user.String(), denom, amount.String(),
    )
    _, err := k.wasmKeeper.Sudo(ctx, k.balanceContractAddr, []byte(msg))
    return err
}
```

### Liens (Trading Allowances)

A **lien** is an on-chain lock on a user's contract balance for a specific denomination. Tokens remain in the user's contract account but are marked as locked, preventing withdrawal or transfer by the user.

When a user sets a trading allowance, the module calls `SudoMsg::Lock` on the balance contract. The contract marks those tokens as locked. When the allowance is reduced, the module calls `SudoMsg::Unlock`.

```
User contract balance:  1500 USDC
  - Available:           500 USDC (user can withdraw/transfer)
  - Locked (lien):      1000 USDC (reserved for trading, enforced by contract)
```

The module maintains its own lien ledger tracking allowances and reservations. The contract enforces the lock — users cannot withdraw or transfer locked tokens through the contract's normal `ExecuteMsg` interface.

### Dual-Lane Order System

#### Slow Lane (On-Chain)

Orders submitted via `MsgPlaceOrder` as a Cosmos SDK transaction. These are processed by the module, validated against the user's confirmed lien, and emitted as events for the offchain engine to pick up.

**Use cases:**
- First trade (atomic with `MsgSetTradingAllowance` in the same tx)
- Increasing position size (atomic with `MsgSetTradingAllowance`)
- CosmWasm contract interactions
- Any order that must be atomic with a lien change

**Latency:** One block (~5-6s on Cosmos SDK chains).

#### Fast Lane (Offchain)

Orders submitted directly to the engine's REST/WebSocket API. The engine validates against its local cache of confirmed allowances and matches immediately.

**Use cases:**
- All trading activity within an already-confirmed allowance
- Market making, arbitrage, and latency-sensitive strategies

**Latency:** Sub-second (engine matching speed).

**Constraint:** The engine only accepts fast-lane orders up to the user's **confirmed** (on-chain) allowance. There is no optimistic acceptance beyond the confirmed lien — this eliminates the settlement risk of fills that can't settle.

### Settlement

Trade results from the offchain engine are batched and submitted to the module periodically (e.g., every block or every N blocks). The module:

1. Validates each trade against the current lien state
2. Adjusts lien allocations (reduces buyer's quote-side lien, increases buyer's base-side lien, etc.)
3. Calls `SudoMsg::Transfer` on the balance contract to move tokens between counterparties
4. Emits settlement events

Settlement transactions are submitted by an authorized **settlement relayer** (the engine operator or a set of authorized addresses). The module validates every trade cryptographically — the relayer cannot fabricate trades.

## Messages

### MsgSetTradingAllowance

Sets or replaces the user's trading allowance for a given denomination.

```protobuf
message MsgSetTradingAllowance {
  string sender = 1;
  string denom = 2;
  string amount = 3;  // sdk.Int as string
}
```

**Validation:**
- User's available balance in the contract (queried via Sudo) must be sufficient to cover the increase
- Allowance cannot be reduced below the user's currently reserved amount (open orders + unsettled trades)

**State changes:**
- Updates the lien ledger: `(user, denom) → allowance_amount`
- Calls `SudoMsg::Lock` on the balance contract for any increase
- Calls `SudoMsg::Unlock` on the balance contract for any decrease

### MsgPlaceOrder (Slow Lane)

Places an order on the orderbook via the on-chain module.

```protobuf
message MsgPlaceOrder {
  string sender = 1;
  string pair_id = 2;
  Side side = 3;           // BID or ASK
  string price = 4;        // decimal string
  string size = 5;         // decimal string
  OrderType order_type = 6; // LIMIT, MARKET, IOC, POST_ONLY
}
```

**Validation:**
- Pair must exist and be in an active trading state
- User's confirmed allowance minus already-reserved amount must be >= order's required collateral
- Price and size must conform to tick/lot size for the pair

**State changes:**
- Reserves collateral within the user's allowance: `reserved += order_collateral`
- Stores the order in module state (pending pickup by engine)
- Emits `EventOrderPlaced` for the offchain engine to consume

### MsgCancelOrder (Slow Lane)

Cancels an order via the on-chain module.

```protobuf
message MsgCancelOrder {
  string sender = 1;
  string order_id = 2;
}
```

**State changes:**
- Releases reserved collateral: `reserved -= order_collateral`
- Emits `EventOrderCancelled` for the offchain engine to consume

### MsgSettleTrades (Relayer-Only)

Submits a batch of trade settlements from the offchain engine.

```protobuf
message MsgSettleTrades {
  string relayer = 1;
  repeated TradeSettlement trades = 2;
}

message TradeSettlement {
  string trade_id = 1;
  string pair_id = 2;
  string maker_address = 3;
  string taker_address = 4;
  string price = 5;
  string size = 6;
  Side taker_side = 7;
  string maker_fee = 8;
  string taker_fee = 9;
  uint64 trade_seq = 10;
  bytes engine_signature = 11;  // engine signs each trade for authenticity
}
```

**Validation:**
- Relayer must be in the authorized relayer set
- Each trade must have a valid engine signature
- Trade sequence must be strictly monotonic per pair (no gaps, no replays)
- Both counterparties must have sufficient allowance for the settlement

**State changes:**
- Calls `SudoMsg::Transfer` on the balance contract to move tokens between counterparties
- Adjusts reserved amounts within each user's allowance
- Updates per-pair trade sequence counter
- Emits `EventTradeSettled` per trade

### MsgUpdateBalanceContract (Admin/Governance)

Updates the balance contract address. Required for migration or upgrades.

```protobuf
message MsgUpdateBalanceContract {
  string authority = 1;          // governance or admin address
  string contract_address = 2;   // new balance contract address
}
```

## Queries

### QueryTradingAllowance

Returns a user's current allowance, reserved amount, and available-to-trade amount for a denomination.

```protobuf
message QueryTradingAllowanceRequest {
  string address = 1;
  string denom = 2;
}

message QueryTradingAllowanceResponse {
  string allowance = 1;   // total liened amount
  string reserved = 2;    // amount backing open orders / unsettled trades
  string available = 3;   // allowance - reserved (available for new orders)
}
```

### QueryOpenOrders

Returns on-chain (slow-lane) orders for a user, pending engine pickup or cancellation.

### QuerySettlementStatus

Returns the latest settled trade sequence per pair, useful for the engine to know what's been confirmed.

### QueryPairConfig

Returns tick size, lot size, min notional, and trading state for a pair.

### QueryBalanceContractAddress

Returns the currently configured balance contract address.

## CosmWasm Integration

### Module → Balance Contract (SudoMsg)

The module uses `wasmKeeper.Sudo()` to invoke privileged operations on the balance contract. This is the same pattern used by IBC wasm hooks and other native modules. The balance contract's `sudo` entry point is only callable by native modules — not by users or other contracts.

```go
// In the orderbook module's keeper
type Keeper struct {
    wasmKeeper          wasmtypes.ContractOpsKeeper
    balanceContractAddr sdk.AccAddress
    // ...
}

func (k Keeper) transferViaContract(
    ctx sdk.Context,
    from, to sdk.AccAddress,
    denom string,
    amount math.Int,
) error {
    msg := fmt.Sprintf(
        `{"transfer":{"from":"%s","to":"%s","denom":"%s","amount":"%s"}}`,
        from.String(), to.String(), denom, amount.String(),
    )
    _, err := k.wasmKeeper.Sudo(ctx, k.balanceContractAddr, []byte(msg))
    return err
}
```

### External Contracts → Module (Custom Messages)

Other CosmWasm contracts can interact with the orderbook module via custom messages:

```rust
#[cw_serde]
pub enum OrderbookMsg {
    SetTradingAllowance {
        denom: String,
        amount: Uint128,
    },
    PlaceOrder {
        pair_id: String,
        side: Side,
        price: Decimal,
        size: Decimal,
        order_type: OrderType,
    },
    CancelOrder {
        order_id: String,
    },
}
```

These are dispatched via the module's custom message handler registered in `app.go`. The `sender` is the contract address — the module validates against the contract's allowance/balance.

### External Contracts → Module (Custom Queries)

```rust
#[cw_serde]
pub enum OrderbookQuery {
    TradingAllowance {
        address: String,
        denom: String,
    },
    OpenOrders {
        address: String,
        pair_id: Option<String>,
    },
    PairConfig {
        pair_id: String,
    },
}
```

### Composability Example

A CosmWasm vault contract that atomically sets an allowance and places a hedged position:

```rust
fn execute_open_hedge(
    deps: DepsMut,
    env: Env,
    info: MessageInfo,
    usdc_amount: Uint128,
) -> Result<Response, ContractError> {
    // Vault already holds USDC in the balance contract.
    // Set trading allowance and place orders — all atomic.

    Ok(Response::new()
        .add_message(CosmosMsg::Custom(OrderbookMsg::SetTradingAllowance {
            denom: "usdc".into(),
            amount: usdc_amount,
        }))
        .add_message(CosmosMsg::Custom(OrderbookMsg::PlaceOrder {
            pair_id: "ETH/USDC".into(),
            side: Side::Bid,
            price: Decimal::from_str("2000.00")?,
            size: Decimal::from_str("0.25")?,
            order_type: OrderType::Limit,
        }))
        .add_message(CosmosMsg::Custom(OrderbookMsg::PlaceOrder {
            pair_id: "BTC/USDC".into(),
            side: Side::Bid,
            price: Decimal::from_str("60000.00")?,
            size: Decimal::from_str("0.008")?,
            order_type: OrderType::Limit,
        })))
}
```

If any message fails, the entire transaction reverts — including the allowance and any prior orders.

## Offchain Engine Integration

### Engine Reads Module State

The engine syncs with the module via:

1. **gRPC queries** — poll `QueryTradingAllowance` for balance checks
2. **Event streaming** — subscribe to `EventOrderPlaced`, `EventOrderCancelled`, `EventAllowanceChanged` via Tendermint WebSocket or gRPC streaming
3. **Startup sync** — on boot, query all allowances and on-chain orders to rebuild the account cache

This replaces the current Kafka-based `account.updates` flow and the deposit-listener/withdrawal-watcher services.

### Engine Writes to Module

The engine submits `MsgSettleTrades` batches via the authorized relayer. Settlement frequency is configurable — every block for maximum consistency, or every N blocks to reduce chain load.

### Account Cache Synchronization

The engine's `AccountCache` is populated from confirmed on-chain allowances:

```
cache[user, denom] = {
    available: on_chain_allowance - on_chain_reserved - engine_reserved,
    locked: engine_reserved  // amount backing fast-lane open orders
}
```

Where:
- `on_chain_allowance` = total lien from module state
- `on_chain_reserved` = amount backing slow-lane orders (from module state)
- `engine_reserved` = amount backing fast-lane orders (engine-local state)

The engine only accepts fast-lane orders when `available > 0` for the required denomination.

## State

### Module Parameters

```
Key:   ParamsKey
Value: {
    balance_contract: string,       // CosmWasm balance contract address
    settlement_relayers: []string,  // authorized relayer addresses
    engine_pubkey: bytes,           // public key for verifying engine trade signatures
}
```

### Lien Ledger

```
Key:   LienPrefix | user_address | denom
Value: { allowance: Int, reserved: Int }
```

### On-Chain Orders (Slow Lane)

```
Key:   OrderPrefix | order_id
Value: { sender, pair_id, side, price, size, order_type, collateral, status, created_at_height }

Index: UserOrderPrefix | user_address | order_id → order_id
Index: PairOrderPrefix | pair_id | order_id → order_id
```

### Trade Sequence Tracking

```
Key:   TradeSeqPrefix | pair_id
Value: { last_settled_seq: uint64 }
```

### Pair Configuration

```
Key:   PairConfigPrefix | pair_id
Value: { base_denom, quote_denom, tick_size, lot_size, min_notional, min_size, state }
```

State enum: `ACTIVE`, `CANCEL_ONLY`, `HALTED`, `AUCTION`.

## Balance Contract Lock Enforcement

The balance contract is responsible for enforcing locks on the user-facing side. When a user calls the contract's normal `ExecuteMsg::Withdraw` or `ExecuteMsg::Transfer`, the contract must check:

```rust
pub fn execute_withdraw(
    deps: DepsMut,
    info: MessageInfo,
    denom: String,
    amount: Uint128,
) -> Result<Response, ContractError> {
    let balance = BALANCES.load(deps.storage, (&info.sender, &denom))?;

    // User can only withdraw available (unlocked) balance
    let available = balance.total.checked_sub(balance.locked)?;
    if amount > available {
        return Err(ContractError::InsufficientAvailableBalance {});
    }

    // Proceed with withdrawal...
}
```

The `locked` field is only modifiable via `SudoMsg::Lock` and `SudoMsg::Unlock` — which only the `x/orderbook` module can call. This ensures users cannot bypass the lien.

## User Flows

### First Trade (New User)

```
1. User has 1000 USDC in the balance contract (deposited previously via ExecuteMsg).

2. User submits single Cosmos SDK tx:
   - MsgSetTradingAllowance { denom: "usdc", amount: "1000" }
   - MsgPlaceOrder { pair: ETH/USDC, bid, price: 2000, size: 0.5 }

3. Module processes MsgSetTradingAllowance:
   - Queries balance contract via SudoMsg::QueryBalance → 1000 available ✓
   - Calls SudoMsg::Lock { user, denom: "usdc", amount: 1000 }
   - Contract marks 1000 USDC as locked
   - Module updates lien ledger: user → allowance: 1000, reserved: 0

4. Module processes MsgPlaceOrder:
   - Checks allowance (1000) - reserved (0) >= required collateral (1000) ✓
   - Sets reserved: user → 1000
   - Stores order in state
   - Emits EventOrderPlaced

5. Engine picks up EventOrderPlaced:
   - Adds order to the book
   - Updates local cache: user available = 0, locked = 1000

6. Order matches against a resting ask:
   - Engine produces trade
   - Trade goes to outbox → settlement batch

7. Settlement batch submitted via MsgSettleTrades:
   - Module calls SudoMsg::Transfer to move USDC from buyer to seller
   - Module calls SudoMsg::Transfer to move ETH from seller to buyer
   - Adjusts both users' reserved amounts and allowances
   - Adjusts lock amounts via SudoMsg::Unlock / SudoMsg::Lock as denoms change
```

### Subsequent Trades (Within Allowance)

```
1. User sends order via engine API (fast lane)
2. Engine checks: user allowance (1000) - reserved (200) = 800 available ✓
3. Engine reserves 500, matches immediately
4. Settlement batch includes the trade
5. Module settles via SudoMsg::Transfer on balance contract
```

### Reducing Allowance ("Withdrawal")

```
1. User submits MsgSetTradingAllowance { denom: "usdc", amount: "500" }
   (reducing from 1000 to 500)
2. Module checks: new allowance (500) >= reserved (200) ✓
3. Module calls SudoMsg::Unlock { user, denom: "usdc", amount: 500 }
4. Contract unlocks 500 USDC — user can now withdraw it via ExecuteMsg
```

No merkle proofs, no permits, no waiting for operator confirmation. Effective in the same block.

### CosmWasm Contract Interaction

```
1. External contract sends custom messages to the module:
   - OrderbookMsg::SetTradingAllowance { denom: "usdc", amount: 1000 }
   - OrderbookMsg::PlaceOrder { ... }
   - OrderbookMsg::PlaceOrder { ... }

2. Module processes each:
   - Calls SudoMsg::Lock on balance contract for the external contract's address
   - Validates and stores orders
   - Emits events

3. All messages are processed atomically within the contract's execution
4. If any fail, the entire execution reverts (including locks and prior orders)
5. Engine picks up both orders from events
```

## Migration Path from Current Architecture

### Phase 1: Balance Contract Upgrade

1. Add `SudoMsg` entry points (`Lock`, `Unlock`, `Transfer`, `QueryBalance`) to the existing balance contract
2. Add `locked` field to the contract's balance storage
3. Ensure `ExecuteMsg::Withdraw` and `ExecuteMsg::Transfer` check available (unlocked) balance
4. Deploy the upgraded contract

### Phase 2: Module Deployment

1. Implement `x/orderbook` module with lien ledger, Sudo-based locking, and basic messages
2. Register the module in `app/app.go` alongside `x/wasm`
3. Configure the module with the balance contract address
4. Deploy via chain upgrade handler

### Phase 3: Dual-Mode Operation

1. Run both systems in parallel — existing merkle proof path + new module
2. Engine consumes events from both sources
3. Users can choose: use legacy withdrawal flow or set allowance (new)
4. Verify settlement correctness before sunsetting the old path

### Phase 4: Engine Adaptation

1. Replace Kafka-based `account.updates` consumer with gRPC event subscription from the module
2. Replace deposit-listener with module event subscription
3. Remove withdrawal-watcher (reducing allowance now unlocks tokens directly in the contract)
4. Update account cache to source from module state

### Phase 5: Legacy Deprecation

1. Migrate remaining merkle-proof-based reserved balances to lien-based allowances
2. Remove merkle tree infrastructure from account-service
3. Remove deposit-listener and withdrawal-watcher services
4. Remove operator root signing key

## Security Considerations

### Sudo Privilege Scope

Only the `x/orderbook` module can call `SudoMsg` on the balance contract. This is enforced at the Cosmos SDK level — `wasmKeeper.Sudo()` is only accessible to native modules, not to users or other CosmWasm contracts. The balance contract should additionally verify the caller if the CosmWasm runtime provides caller context in the Sudo path.

### Settlement Relayer Trust

The relayer can submit trade settlements but cannot fabricate them — each trade requires a valid engine signature. The module verifies:
- Engine signature over trade data
- Strict monotonic trade sequence (no replays, no gaps)
- Both counterparties have sufficient allowance

A compromised relayer can **delay** settlements (by not submitting batches) but cannot **forge** them. Delayed settlements do not cause loss of funds — the engine continues operating and settlements can be submitted later.

### Engine Compromise

If the engine is compromised, it could match unfavorable trades. However:
- The module enforces that settlements only occur within confirmed allowances
- Users can reduce their allowance at any time to limit exposure
- Per-pair trade sequence tracking ensures the module can detect gaps or irregularities
- The engine signature requirement means forged trades submitted by a third party are rejected

### Lock Bypass Attempts

The contract must ensure that locked tokens cannot be withdrawn or transferred through any `ExecuteMsg` path. All balance-modifying execute handlers must check `available = total - locked` before proceeding. The `locked` field is only modifiable via `SudoMsg`, which is module-only.

Edge cases to guard against:
- **Contract migration**: If the balance contract is migrated (admin changes code), ensure the new code preserves the `locked` field semantics
- **Multiple modules**: If other modules also call Sudo on the balance contract, ensure lock accounting doesn't conflict
- **Reentrancy**: Ensure the balance contract's Sudo handlers cannot be reentered via submessages

### Allowance Griefing

A user cannot set an allowance for tokens they don't have — the module queries the contract's available balance at allowance creation time. They also cannot reduce their allowance below the reserved amount, preventing them from pulling funds out from under open orders.

## What This Eliminates

| Component | Current | With Module |
|-----------|---------|-------------|
| Merkle tree generation | Every root update | Removed |
| Permit signing | Every withdrawal | Removed |
| deposit-listener service | Running 24/7 | Removed |
| withdrawal-watcher service | Running 24/7 | Removed |
| Operator root signing key | Critical secret | Removed |
| Operator trust assumption | Required for withdrawals | Removed |
| Kafka for balance updates | Required | Replaced by gRPC events |
| Separate orderbook balance | User manages two balances | Single contract balance with lien |
| Withdrawal latency | Merkle proof + chain tx + watcher | Same-block (reduce allowance → unlock) |
| First-trade setup | Deposit tx + wait for listener | Single tx (set allowance + place order) |
| CosmWasm composability | Not possible | Full atomic composability via custom messages |

## What This Preserves

| Component | Role |
|-----------|------|
| Balance contract | Authoritative balance ledger (existing) |
| Offchain engine | Sub-second order matching (existing) |
| Account-service | Can be retained for read-model / analytics, but no longer in the settlement path |
| Engine WAL + snapshots | Durability for the offchain matching engine (existing) |
