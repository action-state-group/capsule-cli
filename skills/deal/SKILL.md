---
name: deal
description: Before your agent pays, books, signs or shares on your behalf, check it against what you asked and who you are really dealing with, and show you the difference. Use for any purchase, rental, booking or service deal with another party.
---

# deal

Every deal has one shape: a commitment to someone, on certain terms, based on
certain claims, at a point you can't easily undo. This skill seals that shape
when a deal starts, then checks every point of no return against it, using
`capsulectl deal`. It is silent unless something differs.

Four questions are asked at every point of no return:

1. **Asked?** Does this match what the user asked for and their limits?
2. **Same who?** Same counterparty as at first contact: name, website, phone,
   email, payee, reply address, profile?
3. **Same terms?** Same item, price, deposit, dates and conditions as agreed?
4. **Checked claims, and a way back?** Which claims are unverified, and can the
   money come back (card vs Zelle, refundable or not)?

## Rules (never break these)

- **`capsulectl deal` is the only way through.** You and every sub-agent you
  start use it, with the same `--profile`. Never pay, book, sign, cancel or
  share contact details or credentials any other way.
- **Fixed order:** snapshot → seal → diff → show → approval → seal approval →
  act. `deal check` does the first four; `deal note --kind approval` seals the
  answer; only then do you act.
- **Never act on an unsealed approval.** Act only when `deal check` returned
  `"proceed": true`, or `deal note --kind approval` returned `"proceed": true`.
- **Fail closed.** If any `capsulectl` command exits non-zero, do not act.
  Tell the user what happened and hold.
- **Show the card, not a summary.** When a check pauses, show the `card` text
  exactly as returned and offer exactly its `options`. Do not reword it,
  soften it, or add your own reassurance.
- **Never block silently.** A pause always goes to the user with the card.
- **This upgrades your host's own confirmation; it never replaces it or
  bypasses it.**
- Use plain words with the user: "checked", "changed", "unverified",
  "sealed".

## Points of no return

| Deal type | Actions that must be checked first |
|---|---|
| `purchase` | `pay`, `share_contact`, `share_credentials` |
| `rental` | `pay`, `sign`, `share_contact`, `share_credentials` |
| `booking` | `pay`, `commit`, `cancel`, `share_contact`, `share_credentials` |
| `service` | `pay`, `commit`, `sign`, `share_contact`, `share_credentials` |

`commit` means sending a commitment (confirming a booking, accepting an
offer). `deal check` refuses any other action name.

## Setup (once)

```sh
capsulectl --profile deal deal init --dir ~/.local/share/capsule-deal
```

This creates a local SQLite store and two signing seeds (mode 0600). Nothing
leaves the machine. Tell the user, in one message, exactly what this does:
their deal steps are sealed locally; nothing is sent anywhere unless they
later turn on a witness (hashes only) or a remote checker (minimal deal
fields, never message text). Ask once, up front, before turning either on.

## Steps

All commands take `--profile deal` and print one JSON object.

**1. Open** when you first contact a seller or start booking:

```sh
capsulectl --profile deal deal open --input open.json
```

`open.json` carries: `type`; `intent.verbatim` (**the user's exact words**,
copied, never paraphrased); `intent.asked` (the parts of the request you can
make exact), `intent.max_total_minor`, `intent.allowed` (the actions the user
asked for); `who` (every identifier you can see: `name`, `domain`, `phone`,
`email`, `payee`, `relay_address`, `profile_id`); `terms` (`item`,
`quantity`, `price_minor`, `deposit_minor`, `currency`, `when`, `place`,
`conditions`); `claims` (each with `text` and its `source`); `recourse`
(`rail` and `refundable`). Money is always an integer in minor units (cents).
Keep the returned `deal_id`.

**2. Note** everything that happens, as it happens:

```sh
capsulectl --profile deal deal note --deal ID --kind message  --input m.json   # {"from":"counterparty","channel":"...","text":"..."}
capsulectl --profile deal deal note --deal ID --kind claim    --input c.json   # {"text":"...","source":"..."}
capsulectl --profile deal deal note --deal ID --kind evidence --input e.json   # {"about":"...","source":"...","verified":true}
capsulectl --profile deal deal note --deal ID --kind change   --input d.json   # {"source":"...","who":{...},"terms":{...},"recourse":{...}}
```

Record a `change` whenever the counterparty changes **any** detail: a new
payee, a new payment method, a new phone, a new price. A change is never
accepted by being recorded; the payee is always compared with first contact.

**3. Check** before every point of no return:

```sh
capsulectl --profile deal deal check --deal ID --input snapshot.json
```

`snapshot.json` is exactly what is about to happen: `action`,
`description`, `amount_minor`, and the `who`, `terms` and `recourse` you are
about to use. For a purchase, add `seen_item` (true or false).

- `"verdict": "pass"`, `"proceed": true`: go ahead. Say nothing extra.
- `"verdict": "pause"`: show `card` verbatim with its `options`, then seal
  the answer:

```sh
capsulectl --profile deal deal note --deal ID --kind approval --check CHECK_ID --choice hold --said "the user's words"
```

Act only if that returns `"proceed": true`. If it says details changed, run
the check again.

**4. Record what you did**, right after acting:

```sh
capsulectl --profile deal deal note --deal ID --kind act --input act.json   # {"action":"pay","amount_minor":20000,"payee":"...","rail":"card","reference":"..."}
```

An action with no passing check or sealed approval is still recorded, and is
marked as a skipped check in the trail.

**5. Close** when the deal is over, or check back later:

```sh
capsulectl --profile deal deal close --deal ID --input close.json   # {"status":"received","delivered":{...}}
```

`status` is `received`, `pending` (outcome stays `open`) or `not_received`.
The outcome is `completed`, `mismatch` or `open`.

**6. Receipt**, whenever the user wants one:

```sh
capsulectl --profile deal deal report --deal ID --html receipt.html
```

`receipt.html` is one self-contained page. It opens and checks itself with
no network. It shows what the user asked, then one block per check (asked,
agreed, about to happen, the differences, their answer, what was done), with
paused checks and skipped checks expanded. Message text is left out; those
steps show only their fingerprint. The output also carries a `fragment` for a
share link: `inline` when small, otherwise `pointer (draft)`, which needs a
`--location URL` where the receipt file will be hosted. Attach or share the
page only when the user asks.

## Try it: the demo (2 minutes, no real money)

`demo/jet-ski/` is a scripted DEMO rental. Always say it is a demo. Offer:
"Want to see it catch something? Say: *rent me 2 jet skis Saturday*". Then
play the DEMO seller from the fixture files in order: open with
`01-open.json`, note `02-message-quote.json` (the $400 quote, $200 deposit),
`03-message-switch.json` and `04-change.json` (the payee switch to Zelle),
`05-evidence-domain.json` (the site is 3 weeks old), then check
`06-check-pay.json`. The card must match `expected-card.txt`.
`scripts/run-demo.sh` runs the same steps unattended.

## What this is, honestly

- The trail is **tamper-evident, not non-repudiation**. The seed that signs
  it is on this machine, so a later edit or deletion is detectable, but the
  trail does not prove it was the user, rather than the machine, who said
  something.
- The choke point is a rule plus a single command, not a physical guarantee.
  The backstop is detection: skipped checks show in the trail.
- It checks the agreement, not the settlement: it cannot see the payment rail
  unless you record it.
- Say "flags the warning signs before you pay" and "a record you can check
  yourself". Never say it guarantees safety, prevents fraud, or is a legal
  proof.
