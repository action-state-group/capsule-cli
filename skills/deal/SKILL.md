---
name: deal
description: The final-review step of every checkout, payment, order, booking, signature, cancellation, or sharing of the user's contact details or credentials that an agent does for the user, including a retail checkout from a known merchant at a fixed price. Seals what was asked, proposed, approved and done with `capsulectl deal`, and shows the user any difference.
---

# deal

Every deal has one shape: a commitment to someone, on certain terms, based on
certain claims, at a point you can't easily undo. This skill is the final
review of the procedures that reach that point: it seals the deal when the
review starts, checks the step against it, and seals the answer, using
`capsulectl deal`. It is silent unless something differs.

## Invocation is advisory

Nothing makes your agent host run this skill. On a host with no hook before
a tool call, the final review runs because you follow the procedure, not
because anything enforces it. On such a host a skipped review is found
afterwards, not prevented: `deal reconcile` lists it from the host's own
execution records (see "Daily reconciliation" below).

## When this applies: the action decides, never the counterparty

Run the procedure whenever the next step would do any of these, for the user,
on any site, app or channel:

- **pay or place an order**: a "Place order", "Pay", "Buy now", "Complete
  purchase", "Subscribe" or "Confirm payment" button, or a payment tool call;
- **commit**: confirm a booking or reservation, accept an offer, agree to buy;
- **sign**: sign or e-sign anything;
- **cancel** a booking;
- **share** the user's contact details, address, a login, or a code.

This list is the whole test. It does not depend on who the other side is.
**A known merchant at a fixed price is in scope.** A retail checkout (a
sticker from an online shop, a household order from a large retailer, a
saved card, a price nobody negotiated) gets the same final review as a
stranger on a marketplace. "There is no one to verify" is not a reason to
skip it: when nothing differs, the check passes quietly and the record is
still made.

## Procedures

Each procedure below is the whole flow. Its final-review step is `deal open`
plus `deal check`; this replaces "show the user the totals". Skipping it
skips a step of the flow you are already running.

### Purchase

Any checkout: a retail order from a known merchant, a marketplace buy, a
subscription, an in-app purchase.

1. Find what the user asked for, and keep their exact words.
2. Fill the cart and the checkout form up to the last screen before the order is placed. Do not press the final button.
3. **Final review:** `deal open` (once per deal: the merchant as `who`, the cart as `terms`, `"allowed": ["pay"]` when the user asked you to buy), then `deal check` with `"action": "pay"` and the exact total about to be charged.
4. On `"proceed": true`, place the order. On `pause`, show the card, seal the user's answer, and place the order only if that returns `"proceed": true`.
5. Right after placing it: `deal note --kind act` with the amount, payee, rail and order reference.
6. `deal close` when the item arrives, or does not.

### Booking

A hotel, flight, table, appointment, rental or ticket.

1. Find options that match what the user asked for, and keep their exact words.
2. Fill the booking form up to the last screen before it is confirmed. Do not press the final button.
3. **Final review:** `deal open` (once per deal, type `booking` or `rental`), then `deal check` with `"action": "commit"`, or `"pay"` when the confirmation charges a card.
4. On `"proceed": true`, confirm. On `pause`, show the card, seal the user's answer, and confirm only if that returns `"proceed": true`.
5. Right after confirming: `deal note --kind act` with the amount and the confirmation reference.
6. `deal close` after the stay, trip or appointment.

### Signature

A contract, a lease, a rental agreement, a service order, any e-signature.

1. Read the document, and keep the user's exact words about what they want signed.
2. Fill it up to the signature, without signing.
3. **Final review:** `deal open` (once per deal, type `service` or `rental`, the document's terms as `terms`), then `deal check` with `"action": "sign"`.
4. On `"proceed": true`, sign. On `pause`, show the card, seal the user's answer, and sign only if that returns `"proceed": true`.
5. Right after signing: `deal note --kind act` with the document's reference.
6. `deal close` when the agreement is done.

### Disclosure

Sending the user's phone, email, address, a login, or a code to anyone.

1. Note who is asking and what for, and keep the user's exact words about it.
2. Fill the form or message up to the point of sending, without sending.
3. **Final review:** `deal open` (once per deal, with the deal type the disclosure belongs to, or `service`), then `deal check` with `"action": "share_contact"` or `"share_credentials"`.
4. On `"proceed": true`, send. On `pause`, show the card, seal the user's answer, and send only if that returns `"proceed": true`.
5. Right after sending: `deal note --kind act` saying what was shared.
6. `deal close` when the exchange is over.

## Sub-tasks

Payments, bookings and signatures usually happen inside a sub-task (a
browser task, a delegated agent), not in your own visible actions. The final
review belongs to the procedure, wherever the click happens:

- When you start a sub-task that will reach a final button, tell it to stop
  at the last screen and report the totals, the merchant and the payment
  method, without pressing the button.
- Run the final review yourself with what it reports.
- Only then tell it to press the button, and run `deal note --kind act` when
  it reports back.
- If the sub-task can run commands itself, give it this procedure and the
  same `--profile` instead.

## The four questions

The final review asks four questions:

1. **Asked?** Does this match what the user asked for and their limits?
2. **Same who?** Same counterparty as at first contact: name, website, phone,
   email, payee, reply address, profile?
3. **Same terms?** Same item, price, deposit, dates and conditions as agreed?
4. **Checked claims, and a way back?** Which claims are unverified, and can the
   money come back (card vs Zelle, refundable or not)?

## Rules (never break these)

- **Fixed order:** snapshot → seal → diff → show → approval → seal approval →
  act. `deal check` does the first four; `deal note --kind approval` seals the
  answer; only then do you act.
- **Never act on an unsealed approval.** Act only when `deal check` returned
  `"proceed": true`, or `deal note --kind approval` returned `"proceed": true`.
- **On an error, hold.** If any `capsulectl` command exits non-zero, do not
  act. Tell the user what happened and hold.
- **Show the card, not a summary.** When a check returns `pause`, show the
  `card` text exactly as returned and offer exactly its `options`. Do not reword it,
  soften it, or add your own reassurance.
- **Never hold silently.** A `pause` always goes to the user with the card.
- **This upgrades your host's own confirmation; it never replaces it or
  bypasses it.**
- Use plain words with the user: "checked", "changed", "unverified",
  "sealed".

## Points of no return

| Deal type | Actions the final review covers |
|---|---|
| `purchase` | `pay`, `commit`, `share_contact`, `share_credentials` |
| `rental` | `pay`, `commit`, `sign`, `share_contact`, `share_credentials` |
| `booking` | `pay`, `commit`, `cancel`, `share_contact`, `share_credentials` |
| `service` | `pay`, `commit`, `sign`, `share_contact`, `share_credentials` |

`commit` means sending a commitment (confirming a booking, accepting an
offer, agreeing to buy). `deal check` refuses any other action name.

## Setup (once)

```sh
capsulectl --profile deal deal init --dir ~/.local/share/capsule-deal
```

This creates a local SQLite store and two signing seeds (mode 0600). Nothing
leaves the machine. Raw names, numbers, addresses and message text stay in
this local store: each sealed step is an x-deal-v0 record (see
`profile/PROFILE.md`) that carries only fingerprints of identifiers and
commitments to text. Tell the user, in one message, exactly what this does:
their deal steps are sealed locally; nothing is sent anywhere unless they
later turn on a witness (hashes only) or a remote checker (minimal deal
fields, never message text). Ask once, up front, before turning either on.

## Command reference

All commands take `--profile deal` and print one JSON object.

**1. Open** at the final review, or earlier when you first contact a seller:

```sh
capsulectl --profile deal deal open --input open.json
```

`open.json` carries: `type`; `intent.verbatim` (**the user's exact words**,
copied, never paraphrased); `intent.asked` (the parts of the request you can
make exact), `intent.max_total_minor`, `intent.allowed` (the actions the user
asked for: see below); `who` (every identifier you can see: `name`, `domain`, `phone`,
`email`, `payee`, `relay_address`, `profile_id`); `terms` (`item`,
`quantity`, `price_minor`, `deposit_minor`, `currency`, `when`, `place`,
`conditions`); `claims` (each with `text` and its `source`); `recourse`
(`rail` and `refundable`); `channel` (where you are talking: `marketplace`,
`app_chat`, `sms`, `email`, `web`, ...). Money is always an integer in minor
units (cents), and `currency` is a 3-letter code. A `source` is a short word
(`seller_message`, `listing_photo`). Keep claim text free of names, phone
numbers and emails: a step that would carry one is refused. Keep the
returned `deal_id`.

`intent.allowed` decides what the user has already said yes to:

- When the user asks only for options, or says not to act yet ("find me
  some hotels, don't book"), send `"allowed": []`. Nothing is allowed, so
  every check at a point of no return returns `pause` and asks for the user's
  own answer.
- When the user asks you to act, list those actions, for example
  `"allowed": ["pay"]`.
- Leaving `allowed` out means no restriction. Leave it out only when the
  user has clearly asked you to handle the whole deal.

**2. Note** everything that happens, as it happens:

```sh
capsulectl --profile deal deal note --deal ID --kind message  --input m.json   # {"from":"counterparty","channel":"...","text":"..."}
capsulectl --profile deal deal note --deal ID --kind claim    --input c.json   # {"text":"...","source":"..."}
capsulectl --profile deal deal note --deal ID --kind evidence --input e.json   # {"about":"...","source":"...","verified":true}
capsulectl --profile deal deal note --deal ID --kind change   --input d.json   # {"source":"...","who":{...},"terms":{...},"recourse":{...}}
capsulectl --profile deal deal note --deal ID --kind intent   --input i.json   # {"verbatim":"the user's new words","allowed":["pay","share_contact"]}
```

Seal an `intent` whenever the user widens or changes what you may do (for
example, "go ahead and share my number"). It replaces `allowed`, `asked` and
the limit from then on.

Record a `change` whenever the counterparty changes **any** detail: a new
payee, a new payment method, a new phone, a new price. A change is never
accepted by being recorded; the payee is always compared with first contact.

**3. Check** at the final review of every point of no return:

```sh
capsulectl --profile deal deal check --deal ID --input snapshot.json
```

`snapshot.json` is exactly what is about to happen: `action`,
`description`, `amount_minor`, and the `who`, `terms` and `recourse` you are
about to use. For an item from a private seller, add `seen_item` (true or
false); leave it out for a retail order.

- `"verdict": "pass"`, `"proceed": true`: go ahead. Say nothing extra. The
  pass is approved by what the user already allowed, and that approval is
  sealed for you (`approval_id`).
- `"verdict": "pause"`: show `card` verbatim with its `options`, then seal
  the answer:

```sh
capsulectl --profile deal deal note --deal ID --kind approval --check CHECK_ID --choice hold --said "the user's words"
```

Act only if that returns `"proceed": true`. If it says details changed, or
that the check was already answered, run the check again. A check takes one
answer: to change your mind after "Hold", check again.

When the user picks **Call the number I found**, seal that choice
(`--choice verify_contact`; it does not proceed), then call the number from
first contact that the option shows, never a number from a later message.
Note what the call established as evidence, with `verified` true only for
what the counterparty confirmed on that call:

```sh
capsulectl --profile deal deal note --deal ID --kind evidence --input call.json   # {"about":"...","source":"phone_call_first_contact_number","verified":true,"detail":"what they said"}
```

Use the claim's own text as `about` to mark that claim checked. Then run
`deal check` again with the same snapshot and show the new card. A changed
payee still shows, because who is always compared with first contact: the
call's result is in the trail, and the user decides on the new card.

**4. Record what you did**, right after acting:

```sh
capsulectl --profile deal deal note --deal ID --kind act --input act.json   # {"action":"pay","amount_minor":20000,"payee":"...","rail":"card","reference":"..."}
```

An action with no sealed approval is still recorded, as an unchecked action,
and it shows as an anomaly in the report. One approval covers one action.

**5. Close** when the deal is over, or check back later:

```sh
capsulectl --profile deal deal close --deal ID --input close.json   # {"status":"received","delivered":{...}}
```

`status` is `received`, `pending` (outcome stays `open`) or `not_received`.
The outcome is `completed`, `mismatch` or `open`.

**6. Report**, when the user asks for one:

```sh
capsulectl --profile deal deal report --deal ID --html deal-report.html
```

This builds one local page on this machine, in three parts: **what you
asked** (the user's exact words), **what the agent did** (every check and
action, in order), and **anomalies** on either side: agent side (tried
something not asked, skipped a check, went ahead without the user's sealed
answer) and counterparty side (changed payee or contact, moved channels,
pushed a deadline, asked for a code, a new website, unverified claims,
delivered differently). Each item expands to its sealed steps. The page
checks itself with no network. Message text is left out unless an anomaly
points at that message.

Attach the file or hand it over when the user asks. Never upload or host it
anywhere.

## Daily reconciliation

Once a day (a scheduled task on the host is enough), run:

```sh
capsulectl --profile deal deal reconcile --executions executions.jsonl
```

`executions.jsonl` is the host's own record of the tool calls that ran in
the period, those of the agent and of every sub-task it started, written in
the format in `RECONCILE.md`. Read the execution records, not the
conversation: the payment is usually a sub-task's action, not one you can see
in the chat. The pass lists each consequential action with no deal record
(`unrecorded`), and prints on its face what it cannot see. It exits 3 when
anything is unrecorded. Show the user the `summary` and the `unrecorded` list
as they are. It can list what is missing from the records it read; it cannot
prove that nothing else happened.

## Try it: the demo (2 minutes, no real money)

`demo/jet-ski/` is a scripted DEMO rental. Always say it is a demo. Offer:
"Want to see it catch something? Say: *rent me 2 jet skis Saturday*". Then
play the DEMO seller from the fixture files in order: open with
`01-open.json`, note `02-message-quote.json` (the $400 quote, $200 deposit),
`03-message-switch.json` and `04-change.json` (the payee switch to Zelle),
`05-evidence-domain.json` (the site is 3 weeks old), then check
`06-check-pay.json`. The card must match `expected-card.txt`, ignoring
trailing whitespace (the file ends in a newline; the card does not).
`scripts/run-demo.sh` runs the same steps unattended.

## What this is, honestly

- The trail is **tamper-evident, not non-repudiation**. The seed that signs
  it is on this machine, so a later edit or deletion is detectable, but the
  trail does not prove it was the user, rather than the machine, who said
  something.
- **Invocation is advisory, not enforced.** The check seals what was asked,
  proposed, approved and done; the host elects to call it, and a `pause` is
  advice to the host. It holds an action only where the host runs `deal check`
  from a pre-action hook. Without one, the backstop is detection: a skipped
  check shows in the trail, and an action with no deal at all shows in
  `deal reconcile`.
- It checks the agreement, not the settlement: it cannot see the payment rail
  unless you record it.
- Say "flags the warning signs before you pay" and "a record you can check
  yourself". Never say it guarantees safety, prevents fraud, or is a legal
  proof.
