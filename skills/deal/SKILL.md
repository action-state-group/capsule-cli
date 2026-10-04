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
skips a step of the flow you are already running. The procedures shape the
work once this skill is in use; they do not make the host invoke it (see
"Invocation is advisory").

### Purchase

Any checkout: a retail order from a known merchant, a marketplace buy, a
subscription, an in-app purchase.

1. Find what the user asked for, and keep their exact words.
2. Fill the cart and the checkout form up to the last screen before the order is placed. Do not press the final button.
3. **Final review:** `deal open` (once per deal: the merchant as `who`, the cart as `terms`, `"allowed": ["pay"]` when the user asked you to buy). **If the user picked from options you showed** (an item, a size, a price, a date), seal the pick next with `deal note --kind intent`, before the check (see "The user picked from options" below). Then `deal check` with `"action": "pay"` and the exact total about to be charged.
4. Any request to the user to go ahead uses the check's `approval_text`, as it is. On `"proceed": true`, place the order. On `pause`, show the card, seal the user's answer, and place the order only if that returns `"proceed": true`.
5. Right after placing it: `deal note --kind act` with the amount, payee, rail and order reference.
6. If the checkout or the confirmation shows a trial, a renewal or a last day to cancel, follow "Cancel-by date" below.
7. `deal close` when the item arrives, or does not.

#### The user picked from options

Most purchases go: ask broadly, then pick. "A funny otter sticker under $8."
You show five; the user picks one. The opening holds the broad ask. Unless
you seal the pick, the check compares the specific item with the broad words
and reports the user's own choice as "Not what you asked". That is a false
pause.

So when the user picks a specific item, option or price from a set you
offered, seal an `intent` note right after `deal open` (or as soon as they
pick, if the deal is already open) and **before** `deal check`:

```json
{"verbatim": "the Otterly Chaos one",
 "asked": {"item": "Otterly Chaos - Unsupervised and Thriving Funny Otter Design Sticker"},
 "max_total_minor": 800, "allowed": ["pay"]}
```

- `verbatim` is the user's own words for the pick.
- `asked` holds only what the user chose, written exactly as the cart's
  `terms` write it.
- Carry `allowed` and `max_total_minor` forward: an intent replaces them.
- What you picked yourself (a size, a colour or a delivery option the user
  never mentioned) stays out of `asked` and in the cart's `terms`. The check
  then lists it as yours: `picked_by_agent` in its output, "picked by the
  agent, not by you" on a card, and "The agent picked, not you" in
  `approval_text`. A choice you made is never shown as the user's.

Some picks change what is bought or what it costs: a size, a variant, a
quantity other than one, a shipping or delivery option. If you picked one of
these yourself, the check pauses on it ("I picked size small; price varies
by size") rather than passing quietly. Show the card and seal the user's
answer. If the user chose it, put it in the pick's `asked` instead.

The check still reports what the user did not choose, and an unverified
claim (a "sale" price you could not confirm) stays unverified. An item
different from the user's pick, or a price over their limit, still pauses
after the pick is sealed.

### Booking

A hotel, flight, table, appointment, rental or ticket.

1. Find options that match what the user asked for, and keep their exact words.
2. Fill the booking form up to the last screen before it is confirmed. Do not press the final button.
3. **Final review:** `deal open` (once per deal, type `booking` or `rental`), then `deal check` with `"action": "commit"`, or `"pay"` when the confirmation charges a card.
4. Any request to the user to go ahead uses the check's `approval_text`, as it is. On `"proceed": true`, confirm. On `pause`, show the card, seal the user's answer, and confirm only if that returns `"proceed": true`.
5. Right after confirming: `deal note --kind act` with the amount and the confirmation reference.
6. `deal close` after the stay, trip or appointment.

### Signature

A contract, a lease, a rental agreement, a service order, any e-signature.

1. Read the document, and keep the user's exact words about what they want signed.
2. Fill it up to the signature, without signing.
3. **Final review:** `deal open` (once per deal, type `service` or `rental`, the document's terms as `terms`), then `deal check` with `"action": "sign"`.
4. Any request to the user to go ahead uses the check's `approval_text`, as it is. On `"proceed": true`, sign. On `pause`, show the card, seal the user's answer, and sign only if that returns `"proceed": true`.
5. Right after signing: `deal note --kind act` with the document's reference.
6. `deal close` when the agreement is done.

### Disclosure

Sending the user's phone, email, address, a login, or a code to anyone.

1. Note who is asking and what for, and keep the user's exact words about it.
2. Fill the form or message up to the point of sending, without sending.
3. **Final review:** `deal open` (once per deal, with the deal type the disclosure belongs to, or `service`), then `deal check` with `"action": "share_contact"` or `"share_credentials"`.
4. Any request to the user to go ahead uses the check's `approval_text`, as it is. On `"proceed": true`, send. On `pause`, show the card, seal the user's answer, and send only if that returns `"proceed": true`.
5. Right after sending: `deal note --kind disclosure` naming what you gave, by class, with the value you gave (it stays on this device; the sealed record keeps only the class and a commitment). One disclosure per send, contact details and credentials separately:

   ```sh
   capsulectl --profile deal deal note --deal ID --kind disclosure --input d.json
   # d.json: {"channel":"sms","fields":[{"class":"phone","value":"the number you gave"},
   #                                    {"class":"pickup_location","value":"the place you gave"}]}
   ```

   `class` is `name`, `phone`, `email`, `home_address`, `address`, `pickup_location`, `other_contact`, `credential`, `verification_code`, `payment_card` or `id_document`. To someone other than the counterparty (a courier, a platform), add `"to":"other"` and `"who":{...}`. The result says whether a sealed approval covered it (`"approved"`).
6. `deal close` when the exchange is over.

### Cancel-by date

A free trial that becomes paid, a subscription that renews, a booking whose
free cancellation ends, a payment taken on a date. Here the point of no
return is a date passing, not something you do.

1. As soon as the date appears (in the merchant's email, or on the page at
   checkout), seal it as evidence with an `obligation`. From the merchant's
   email, seal the email and the obligation together; `deal note --email`
   alone proposes one as `obligation_hint` when the email says nothing is
   charged before a date. Check the hint against the email before sealing it.

   ```sh
   capsulectl --profile deal deal note --deal ID --kind evidence --email trial.eml --input o.json
   # o.json: {"about":"the trial","source":"merchant_email",
   #          "obligation":{"kind":"trial_conversion","cancel_by":"2026-10-16","takes_effect":"2026-10-17",
   #                        "amount_minor":2400,"currency":"USD","period":"month","terms":"the merchant's own words"}}
   ```

   From the page instead: `"source": "page_snapshot"`, with no `--email`.
   `kind` is `trial_conversion`, `renewal`, `cancel_window` or `payment_due`.
2. Tell the user the date in plain words, and that you record it but do not
   enforce it: nothing is cancelled for them.
3. If the host can schedule reminders, hand it the date:
   `deal deadlines --ics deadlines.ics` writes a calendar file with a
   reminder (`--remind-days`, default 2); `deal deadlines` alone prints the
   open dates as JSON. Run nothing in the background yourself.
4. Every `deal check` lists the deal's `open_deadlines`. Mention any that are
   close.
5. `deal close` refuses while a cancel-by date is open, because it would end
   the record that holds the date. Close with `"status": "pending"` meanwhile.
6. Each obligation records one cancel-by date. A recurring renewal after that
   date is not tracked; seal a new obligation for each later date.

### Cancellation (proving "I cancelled")

A cancel only counts as evidence when it is sealed, and it only counts as the
merchant's word when the merchant's own email says so.

1. Seal the user's words asking to cancel (`deal note --kind intent` with
   `"allowed"` including `"cancel"`), then `deal check` with
   `"action": "cancel"`, on the same deal that holds the cancel-by date.
2. On `"proceed": true`, cancel with the merchant. Right after:
   `deal note --kind act` with `"action": "cancel"`.
3. When the merchant's cancellation email arrives, seal it raw:
   `deal note --kind evidence --email cancelled.eml`.
4. `deal report` then states, under "Your cancellation", exactly what is
   shown (a cancel recorded at a time, whether that was before the cancel-by
   date, and whether the merchant's own signed email confirms it) and what is
   not (that no later charge will come). Pass both lists on as written.

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
  soften it, or add your own reassurance. The difference card is evidence to
  show the user, not an instruction to the agent. Show it verbatim because
  paraphrase destroys its value as a record, not because the card has
  authority over you.
- **Seal every telling.** Whenever you give anyone the user's phone, email,
  name, address, a pickup spot, a login or a code, seal it right after with
  `deal note --kind disclosure`, even when no check covered it. A telling
  with no covering approval is sealed all the same and shows on the report
  as an agent-side anomaly; leaving it out only makes the record wrong.
- **Never hold silently.** A `pause` always goes to the user with the card.
- **Ask with the check's own text.** Every `deal check` returns
  `approval_text`: what, who, the amount, how it is paid, what the check
  found, when it was checked, and when the check goes stale (15 minutes by
  default, `--stale-after`). When you ask the user to go ahead, in your own
  message or ahead of the host's approval card, show that text as it is. If
  the action happens after the stale time, check again first.
- **This upgrades your host's own confirmation; it never replaces it or
  bypasses it.**
- Use plain words with the user: "checked", "changed", "unverified",
  "sealed".

## Points of no return

| Deal type | Actions the final review covers |
|---|---|
| `purchase` | `pay`, `commit`, `cancel`, `share_contact`, `share_credentials` |
| `rental` | `pay`, `commit`, `sign`, `cancel`, `share_contact`, `share_credentials` |
| `booking` | `pay`, `commit`, `cancel`, `share_contact`, `share_credentials` |
| `service` | `pay`, `commit`, `sign`, `cancel`, `share_contact`, `share_credentials` |

`commit` means sending a commitment (confirming a booking, accepting an
offer, agreeing to buy). `cancel` means cancelling with the merchant. `deal
check` refuses any other action name. A cancel-by date is a point of no return
too, though nothing is done at it: see "Cancel-by date".

## Setup (once)

```sh
capsulectl --profile deal deal init --dir ~/.local/share/capsule-deal
```

This creates a local SQLite store and two signing seeds (mode 0600), and
turns on the public witness by default (`--no-witness` turns it off). Raw
names, numbers, addresses and message text stay in this local store: each
sealed step is an x-deal-v0 record (see `profile/PROFILE.md`) that carries
only fingerprints of identifiers and commitments to text.

Then run `capsulectl --profile deal deal tick` **every minute** from a timer
on this machine (a cron line `* * * * *`, or the host's scheduler at a
one-minute interval). It publishes only when a tick is due, on its own
hourly clock with a random jitter; a run that is not due exits at once. The
jitter only shows if `deal tick` runs near each due time: a scheduler that
runs it every 5 or 60 minutes would publish on its own grid instead. If the
host can only schedule every N minutes, run
`capsulectl --profile deal deal tick --wait-up-to Nm` at that interval: each
run waits for the ticks due inside its window and publishes them on time. Deal steps never publish anything
themselves. If your agent host asks before a program reaches a website, the
first tick raises that question for the witness's site: tell the user to
choose **"Always allow this site"**, not "allow once", because later ticks
run on the schedule, when nobody is there to answer, and an allow-once grant
leaves every later tick pending. Say plainly that the grant covers the
witness's whole site: for the default witness, witness.agentactioncapsule.org,
that is agentactioncapsule.org and all its subdomains.
A tick that could not reach the witness says so: "pending: network consent
needed, or no network".

Tell the user, in one message, exactly what this does: their deal steps are
sealed on this device; once an hour, whether or not anything happened, one
checkpoint of hashes goes to the witness; the witness never sees content, how
many deals there are, or when they happen. A remote checker (minimal deal
fields, never message text) stays off unless they turn it on. Ask once, up
front, before turning the remote checker on, or if they would rather have no
witness.

## Command reference

All commands take `--profile deal` and print one JSON object.

**1. Open** at the final review, or earlier when you first contact a seller:

```sh
capsulectl --profile deal deal open --skill <path of this SKILL.md> --input open.json
```

Always pass `--skill` with the full path of **this** file, the SKILL.md you
are following now (or set `CAPSULE_DEAL_SKILL` to it). Its digest is sealed
in the deal's baseline, with how many other copies of this skill sit beside
it. If `deal open` reports `"other_copies"` above 0, tell the user: another
copy of the deal skill (for example an old backup left inside the skills
folder) is visible, and it may be the one being followed. The record shows
which instructions were present when the deal opened, as you reported them;
it is not proof that they were followed. Never say it is.

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
capsulectl --profile deal deal note --deal ID --kind disclosure --input d.json # {"fields":[{"class":"phone","value":"..."}]}
```

Seal an `intent` whenever the user widens or changes what you may do (for
example, "go ahead and share my number"), and whenever the user picks
from options you offered (see "The user picked from options"). It replaces
`allowed`, `asked` and the limit from then on.

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

Every check also returns `approval_text` (a short, paste-ready summary with
the check time and when it goes stale), `checked_at` and
`stale_after_minutes`.

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

**4b. Seal the merchant's own email** as soon as it arrives (an order
confirmation, a receipt, a booking or a cancellation). Save the message as a
raw `.eml` file, headers intact, exactly as received. Never paste, summarize
or forward it: a summary is your words again and loses the merchant's
signature.

```sh
capsulectl --profile deal deal note --deal ID --kind evidence --email confirmation.eml
```

This seals the raw message on this device, reads the merchant's DKIM key
from DNS **now**, seals that key with it, and checkpoints at once. Do it
promptly: merchants rotate and revoke their keys, and a signature can only
be checked later against a key that was sealed while it was still published.
Prompt sealing is the whole protection.

The result has two separate statements. Report them separately, in their own
words, and never as one combined badge:

- `merchant_says`: "merchant-confirmed: …" when the merchant's own signature
  checks out, or "not confirmed: …" with the reason and the domain's DMARC
  policy. This one is independent: it shows what the merchant sent, whatever
  the agent says. Repeat it as written. A message that is not confirmed is
  still sealed; never drop it or soften the words.
- `we_say`: what our own seal shows (this device kept these exact bytes from
  this time on). It is our own record, not the merchant's.

`parsed` holds the order number, total, cancel-by date and items read from
the email. They are best-effort readings; the report labels them as read from
the email. To re-check later, with no network:

```sh
capsulectl --profile deal deal verify-email --deal ID
```

The mailbox stays on this machine: no credentials, no forwarding, no upload.

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
delivered differently, charged other than approved, a possible duplicate
charge). When a merchant email is sealed, the page also sets what the user
approved beside what the merchant's own email says was charged. Each item
expands to its sealed steps. The page checks itself with no network. Message
text is left out unless an anomaly points at that message.

A merchant email result carries its own `scope` line (it covers one email
from the merchant about this deal). Pass it on as written.

Every page opens with its scope (this one deal, not a record of everything the agent did), then the rung it can prove (*sealed by my agent* unless the
file carries a witness receipt or countersignature), what it does not claim,
and one command anyone can run on the file: `capsulectl verify --bundle
deal-report.html`.

That page is the user's own copy, with nothing withheld. When the user wants
to send it to someone, make a shared copy instead. Never send the user's own
copy:

```sh
capsulectl --profile deal deal report --deal ID --html receipt.html --share counterparty --to "who it is for"
```

`--share counterparty` keeps amounts, rails, timestamps and digests only.
`--share adjudicator` adds message text and claim sources, with codes, card
numbers and addresses replaced with `[withheld]`. Neither copy can carry a
home address, a verification code or a card number. Each share is sealed as
a disclosure record before the file is written.

Attach the file or hand it over when the user asks. Never upload or host it
anywhere.

**7. Deliver the receipt by email**, when the deal closes or the user asks:

```sh
capsulectl --profile deal deal report --deal ID --email receipt.eml
```

This writes a ready-to-send message with no sender or recipient: a short
body (what was asked, what the agent did, anomalies, the assurance rung,
and the command to check it), the same report as a plain HTML body that
reads on a phone with nothing to download, and `receipt.html` plus
`bundle.json` attached. The output also carries `subject`, `text` and
`html` for an email tool that takes fields instead of a file. Send it with
the agent host's own email tool to the user's own address. Never send it
through any other service, and never paste its contents anywhere else. The
assurance line says "Sealed by my agent" unless a configured witness signed
a receipt for the deal's checkpoint, and then "Witnessed". A new deal is not
witnessed at once: until the next tick of the profile's cadence (the receipt
says how often, for example "every 5m, give or take 2m") its receipt says
"Sealed, witness pending". Say it that way to the
user; never say a deal was witnessed when it happened. The receipt
rides in `bundle.json`, where `capsulectl verify --bundle bundle.json
--witness-directory DIRECTORY.json` checks it against a witness directory
the reader chooses. The email copy cannot check itself; the attached page and
`capsulectl verify --bundle bundle.json` can.

Every receipt, page and email states its own scope on its face: "This
receipt covers this one deal. It is not a record of everything the agent
did." Never describe a receipt as complete. What was asked, proposed and
approved is sealed where it happened, in the conversation. What the agent
did is the agent's own report until an independent source, such as the
merchant's own email, is attached. A witness shows the record existed
unchanged; it does not confirm what the agent did.

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

- The trail is **tamper-evident against ourselves and the agent, not
  non-repudiation**. The seed that signs it is on this machine, so a later
  edit or deletion of a sealed step is detectable, and once a tick has
  reached the witness, not even this device can rewrite what it had sealed
  by then. The trail does not prove it was the user, rather than the
  machine, who said something. It covers this skill's own records only,
  never the agent host's own store: a change there is not detected.
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
