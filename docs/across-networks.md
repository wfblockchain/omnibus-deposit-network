# Across networks

The model has one home: a joint account at the central bank and one chain
where the operator keeps the positions ledger. This page covers how it reaches
beyond that home in three directions:

- **Other chains.** A bank's token can be held and used on public and partner
  chains.
- **Other payment rails.** Fedwire, FedNow, RTP and payees outside the
  network.
- **Other banks, operators and currencies.**

For each it separates what the code does today from what is design only. The
call-level detail is in [contract-flows.md](contract-flows.md).

## What never leaves home

Whatever network a token travels to, four things stay on the home chain and
at the central bank:

| Stays home | Why |
|---|---|
| The reserves, in the joint account | A token abroad is the same bank liability; its backing is counted as supply abroad (`remoteSupply`), never moved |
| The positions ledger | One ledger, one reconciliation against one Fed statement |
| Cross-bank conversion (A-dT → B-dT) | `moveBacking` needs the ledger. Abroad, a bank's token moves only between that bank's admitted holders |
| Funding, defunding, interest | They follow the Fed, which only sees the joint account |

## 1. Other chains

### Shape: hub and spoke

```mermaid
flowchart TB
    subgraph HOME["Home chain · operator"]
        L["OmnibusLedger"]
        HM["CrossChainMessenger (hub)<br/>corridorCap · outstanding per bank per chain"]
        TA["A-dT"]
        TB["B-dT"]
        TA --- L
        TB --- L
        HM -- "recordRemote ±" --> L
    end
    subgraph X["Public chain X"]
        MX["Messenger (spoke)<br/>supplyCap per bank"]
        RAX["A-dT (RemoteBankToken)"]
        RGX["Bank A registry"]
        DVX["DvPSettlement"]
        SX["Tokenized fund"]
    end
    subgraph Y["Layer-2 chain Y"]
        MY["Messenger (spoke)"]
        RAY["A-dT"]
        RBY["B-dT"]
    end
    subgraph Z["Partner chain Z"]
        MZ["Messenger (spoke)"]
        RBZ["B-dT"]
    end
    HM <-. "attested messages" .-> MX
    HM <-. "attested messages" .-> MY
    HM <-. "attested messages" .-> MZ
    MX --> RAX
    DVX --- SX
    DVX --- RAX
```

Other chains talk only to home, never to each other. Moving A-dT from
chain X to chain Y is two hops through home. In exchange, home always knows
exactly how much of each bank's supply sits on each chain (`outstanding`).

### Trust: two keys, and a budget per chain

A chain is only as safe as what can mint on it. The messenger limits both:

- **Who can mint.** Every mint, on every chain, needs a strict majority of
  the operator's attesters **and** the issuing bank's own key. The operator
  alone cannot create a bank's deposits anywhere, and neither can the bank
  alone.
- **How much.** Home caps each bank's corridor to each chain (`corridorCap`,
  closed until governance opens it). Each chain caps each bank's supply there
  (`supplyCap`). A chain can never send home more than home sent to it. If a
  chain, its attesters or both keys were lost, the loss stops at those caps.
- **Who can hold.** A bank admits holders on each chain in its own registry
  there. Revocations made at home follow the token to every chain
  (`sendRevocation`); admissions never do, so a compromised hub can restrict
  but never admit.

Sizing a corridor cap is a business decision: it is what a bank is prepared
to lose if that chain fails.

### Connecting a new chain

What the code supports today, for an EVM chain:

1. **Operator:** deploy the spoke messenger and a DvP venue with
   `script/DeployRemote.s.sol`. It needs the timelock, guardian, attesters,
   threshold, the home and local domain ids, and the home messenger.
2. **Each bank that wants to be there:** deploy its registry and
   `RemoteBankToken` with `script/DeployBank.s.sol --sig "runRemote()"`. The
   script grants the messenger the registrar role on the bank's registry, so
   revocations from home can apply.
3. **Governance, through the timelock, on both sides:**
   - `setRemote(domain, messenger)` on home and on the new chain;
   - `setToken(member, token)` for each bank;
   - `setIssuerAttester(member, bankKey)` with the bank's key for that chain;
   - `setSuspense(member, wallet)` for returns that nobody can hold;
   - `setSupplyCap(member, cap)` on the new chain;
   - `setCorridorCap(member, domain, cap)` at home. This is the step that
     opens the corridor.
4. **Close the deployment:** `script/VerifyRoles.s.sol` must pass (governance
   holds every admin; the deployer holds nothing).
5. **Operate:** set the attesters' finality rule for that chain. Each bank
   admits its holders there.

Nothing changes for chains already connected, and no existing contract is
redeployed.

### Finality is per chain

Attesters sign a message only once its burn is final on the source chain.
The destination cannot check this itself, so the rulebook sets the rule for
each chain:

| Source chain type | A reasonable finality rule |
|---|---|
| The home chain (BFT consensus) | Final on commit |
| Ethereum mainnet | The finalized checkpoint (about 13 minutes) |
| A rollup | Its batch finalized on the underlying chain; or, by rulebook decision, the sequencer's confirmation within a smaller cap |
| A permissioned partner chain | Its own consensus finality, as its operator documents it |

There is deliberately no "fast transfer" in which attesters sign before
finality.

### How messages travel

| Transport | Status | What it changes |
|---|---|---|
| **Operator attesters** (m of n) + issuer key | **Built** (`CrossChainMessenger`, envelope version 3) | — |
| **Light-client verification** (for example IBC between chains that support it) | Design | Proof that the burn happened, checked against the source chain's consensus, replaces trust in the attester majority for that corridor. Adopt per corridor, once that light client has been audited. The bank's key still co-signs every mint. |
| **Third-party message networks** | Design, by exception | Only as carriers of already co-signed messages. They never receive mint rights over a bank's token. |

The rule behind the table is that mint power stays with the operator and the
issuing bank. A transport can only carry a message or prove one; it cannot
authorize one.

### Non-EVM chains

The contracts are Solidity. A non-EVM chain needs a port of the spoke
messenger, the remote token and the registry that keeps the same checks: two
keys, supply cap, used nonces, holder policy, freeze, forced transfer,
recovery and pause. The home side does not change. None is built.

### What a public chain reveals

On a public chain, balances and transfers of a bank's token are visible to
anyone. The design keeps client payments on the home chain and takes tokens
abroad only for what must happen there, such as settling against an asset
issued on that chain.

## 2. Other payment rails

### Fedwire and FedNow: only at the edges

The joint account is an account at the Fed, so only the Fed's own transfer
services move reserves in or out of it. Payments inside the network never use
them.

| Event | Rail |
|---|---|
| A bank funds its position | pacs.009 master → joint over Fedwire, or a FedNow liquidity management transfer when Fedwire is closed |
| A bank defunds | pacs.009 joint → master, in Fedwire hours |
| Interest on the joint account | The Fed's credit to the joint account |
| Reconciliation | The Fed's statement and intraday reports for the joint account (camt.053, camt.052) |
| Every payment between members | None: ownership moves inside the joint account |

### RTP and other instant rails

The joint-account structure is the one prefunded instant-payment systems
already use: banks prefund an account at the Fed, and the system's ledger
moves ownership inside it. This network differs in what the ledger can do:

- tokens that banks and their clients hold and program against (holds,
  freezes, DvP);
- no per-payment cap;
- netting as well as gross settlement;
- the same dollars usable on other chains.

The message flow maps one to one onto ISO 20022, so a bank's payment hub
treats the network as one more rail:

| Network | ISO 20022 |
|---|---|
| `pay` | pacs.008 |
| `accept` / `reject(code)` | pacs.002 ACSC / RJCT with the reason code |
| `returnPayment` | a new credit referencing the original |
| `requestReturn` | camt.056, answered with camt.029 |

### Payees outside the network

The network pays members only. The hub refuses an order to a non-member's
routing number (RC01). A bank pays everyone else from its own systems over
RTP, FedNow, Fedwire or ACH as it does today. `services/payments-svc` routes
between book transfer, instant on-network and netting. Adding an off-network
route there is the natural next step, but it is not built.

## 3. Other banks, operators and currencies

### Adding a member bank

Existing members see no change:

1. The bank deploys its registry and token (`DeployBank.run`), with its own
   issuer, compliance, pauser and registrar keys.
2. The operator's timelock admits it: `admitMember` with its token, its
   gateway key, a different approver key, and whether it screens inbound
   payments. Then `setLimits` (issuance cap, prefund
   requirement), `registerWallet` for its settlement wallet, and, per chain,
   `setToken` and `setIssuerAttester`.
3. `VerifyRoles` passes.
4. The bank funds its position (pacs.009) and starts tokenizing.

### A second network or currency

Each network is its own home: its own central-bank account, operator, ledger
and chain. Two networks' tokens are different liabilities over different
reserves. The contracts are currency-neutral apart from naming and the cent
rule for defunds (`CENT`, 6 decimals), so a network in another currency
deploys the same set with its own operator.

Connecting two networks is design, not code:

- **Same chain, two spokes.** Network 1 can connect network 2's home chain
  as one of its spokes, and the reverse. Both networks' tokens can then sit
  on the same chain, each capped and co-signed by its own issuer.
- **PvP there.** A payment-versus-payment contract on that chain could swap
  a network-1 token for a network-2 token atomically, the way
  `DvPSettlement` swaps a security for cash. Across currencies the swap
  carries an FX rate agreed off-chain.
- **What it would not do.** It would not merge ledgers or move reserves
  between central banks. Each network keeps reconciling to its own account.

## Summary

| | Built | Design only |
|---|---|---|
| Other chains | EVM spokes, two-key mint, caps, bounce, revocation, DvP, deploy and verify scripts | Light-client corridors, non-EVM ports, third-party carriers |
| Other rails | Fedwire/FedNow funding and defunding (simulated), ISO 20022 throughout, RC01 for non-members | Off-network payouts from the hub |
| Other banks | Member admission through the timelock, per-bank deploy | — |
| Other networks | — | Shared-chain spokes, PvP between networks' tokens |
