# Operating the omnibus contracts

Governance, emergency controls, deployment, migration and assurance for
`contracts/src/omnibus`. The rule behind all of it: **restrictive actions are
fast, permissive actions are slow**, and no key that deploys a contract keeps
any power over it.

## Who holds what

| Contract | Fast (restrictive), held by a guardian or the bank | Slow (permissive), held by the timelock | Admin |
|---|---|---|---|
| OmnibusLedger | `GUARDIAN_ROLE`: suspend a member | `GOVERNOR_ROLE`: admit, reinstate, change operator/approver, limits, wallets | The operator timelock |
| PaymentRouter | `PAUSER_ROLE`: pause payments (expiry still works) | window and role changes | The operator timelock |
| OmnibusNetting | `PAUSER_ROLE` | operator changes | The operator timelock |
| CrossChainMessenger | `PAUSER_ROLE` (guardian): pause, lower a corridor or supply cap, disable an attester | add attesters, raise threshold or caps, issuer keys, remotes, tokens, suspense wallets | The operator timelock |
| BankToken / RemoteBankToken | `PAUSER_ROLE` (the bank): stop everything, forced transfers included; `COMPLIANCE_ROLE`: freeze (works while paused), forced transfer, recovery | holder policy replacement | the bank's own governance |
| HolderRegistry | the bank's registrar admits and revokes; home broadcasts revocations to other chains | | the bank's own governance |
| DvPSettlement | `PAUSER_ROLE`: stop matching, funding and settlement (refunds and withdrawals still work) | nothing else: no path to escrowed funds | The operator timelock |

Every admin is an OpenZeppelin `AccessControlDefaultAdminRules` role: it can
never be granted directly, and handing it over is two-step and waits out the
admin delay.

**Parameters.** These are starting points taken from practice, not rules: a
TimelockController with a minimum delay of 48 hours (Sky's governance delay)
for routine changes; 7 days for member admission and attester changes, the
long-executor pattern Aave uses; an admin delay of at least 3 days on every
contract; the timelock's optional admin renounced. The guardian is a multisig
separate from the timelock's proposers.

**Why forced transfers stop during a pause.** Pause is the kill switch for a
compromised compliance key as much as for a bug; the audit of ERC-3643 found
forced transfers bypassing pause (finding M-09, Sep 2026) and the standard's
fix makes them revert. Freezing, which only restricts, keeps working.

## Cross-chain controls

- **Two keys per mint:** the operator's attester threshold plus the issuing bank's own
  key on that chain. A key cannot hold both roles.
- **Threshold:** always a strict majority of enabled attesters and never more
  than their number. Raise it before adding an attester, lower it before
  removing one; the guardian can remove a suspect attester at once within
  these rules.
- **Caps:** per corridor at home, per member supply on each other chain.
- **Undeliverable transfers** (recipient not admitted, token unknown) are
  bounced by anyone with the original attestations; the nonce is spent and a
  RETURN re-mints at the source. A RETURN nobody can hold lands in the bank's
  suspense wallet on that chain. A paused token makes a message wait; it is not
  a reason to bounce.
- **Revocations** broadcast from home apply on arrival; admissions stay local
  to each chain.

## Deploying

The scripts in `contracts/script` deploy with the deployer as temporary admin
and zero admin delay, give every operational role to its holder, and begin
handing each admin to governance:

```
forge script script/DeployHome.s.sol   --rpc-url $RPC --broadcast   # the operator, home chain
forge script script/DeployBank.s.sol   --rpc-url $RPC --broadcast   # a bank, home chain
forge script script/DeployBank.s.sol   --sig "runRemote()" ...      # a bank, another chain
forge script script/DeployRemote.s.sol --rpc-url $RPC --broadcast   # the operator, another chain
```

Then governance accepts, through the timelock, `acceptDefaultAdminTransfer`
on every contract (OpenZeppelin requires the acceptance in a later block than
the handover), raises the admin delay with `changeDefaultAdminDelay`, and
schedules the configuration only it may do: member admission, corridor and
supply caps, issuer keys, suspense wallets, remotes and tokens.

**Closing a deployment.** `script/VerifyRoles.s.sol` fails unless, on every
contract, the admin is governance, no handover is pending, and the deployer
holds no role at all. A deployment is not finished until it passes: deployer
keys left holding admin rights are how Stake DAO (Jun 2026) and Wasabi
(Apr 2026) were drained. The full lifecycle (deploy, verify fails, timelock
accepts after its delay, verify passes) is exercised by
`test/omnibus/Deploy.t.sol` and was run with `--broadcast` against anvil.

Manifests are written to `contracts/deployments/<chainid>-<name>.json`.

## Replacing a contract

The contracts are immutable; a flaw is fixed by deploying a successor. For the
router, netting, messenger and DvP venue this needs no new capability:

1. The guardian pauses the contract (the messenger on every chain it touches).
2. Let in-flight work end: held payments settle or expire, netting
   obligations settle or expire, every cross-chain message is delivered or
   bounced, DvP trades settle or lapse. Nothing is migrated mid-flight.
3. Deploy the successor with the scripts; the timelock re-grants the ledger's
   `ROUTER_ROLE`, `NETTING_ROLE` or `MESSENGER_ROLE` to it and revokes the old
   one's.
4. Reconcile: `invariantsHold()`, the Fed attestation, per-corridor counts.

**Replacing a bank's token.** Its backing never moves; the old supply is
re-created on a fixed successor from the old contract's own state:

1. The bank pauses its ticker. Payments holding its tokens settle or expire
   first (the ledger refuses while any hold is open), and inbound payments to
   the bank are answered or expire (accepting one mints the paused ticker).
   Cross-chain messages for the bank are delivered or bounced.
2. The bank deploys the successor (`DeployBank.s.sol`, same registry and
   roles) and keeps it paused.
3. The timelock calls `ledger.beginTokenReplacement(memberId, successor)`. The
   ledger checks both tickers are paused, no hold is open and the successor
   has no supply, and records the old supply as the target.
4. Anyone calls `successor.migrateBalances(holders)` in batches, with holders
   taken from the old ticker's Transfer events. Each balance, freeze and
   blocked key is read from the paused predecessor itself; blocks and freezes
   carry over even for zero balances. The ledger stops the total at the old
   supply, so nothing can be minted beyond it, and a holder is never migrated
   twice.
5. Anyone calls `ledger.completeTokenReplacement(memberId)`. It succeeds only
   when every unit of the old supply exists on the successor; a forgotten
   holder blocks it. The member's ticker becomes the successor and the old one
   is retired: nothing can ever move it again, even if its bank unpauses it,
   so no unbacked copy of a deposit can circulate.
6. The timelock points the home messenger at the successor
   (`setToken`); the bank unpauses the successor. `invariantsHold()` is true
   throughout, because the old ticker carries the backing until the swap.

The timelock can `cancelTokenReplacement` before completion; the successor is
then retired and the member keeps its ticker. Mint references (`refUsed`) do
not carry over, so the bank's core system must not reuse them.

## Assurance

| Check | How | State |
|---|---|---|
| Unit, scenario and fuzzed invariant tests | `forge test` | 268 pass; invariants cover backing = supply at home and abroad, corridor counts, venue escrow, bounces |
| Static analysis | `slither .` (config in `contracts/slither.config.json`) | no High or Medium; 2 false positives annotated in `DvPSettlement` with the reason |
| Coverage | `forge coverage --ir-minimum` | lines 78-94% per omnibus contract, branches 30-54%; `--ir-minimum` under-reports inlined code |
| Deployment | `VerifyRoles` after every deployment | exercised in tests and on anvil |
| Still to do | mutation testing (slither-mutate or vertigo-rs; `forge --mutate` times out under via-IR), symbolic checks of the ledger invariant (Halmos, Certora), and an independent audit | open |
