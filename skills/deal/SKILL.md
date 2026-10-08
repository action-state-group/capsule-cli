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
2. Fill the cart. Before you type any of the user's own details into the checkout (name, email, delivery address, phone), follow "Disclosure" below for them, on this deal (`deal open` it first): a delivery form is a disclosure to the merchant. Then fill the checkout up to the last screen before the order is placed. Do not press the final button.
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
- `allowed` and `max_total_minor` carry forward UNCHANGED: copy the user's
  values into the note. An intent may PROPOSE a change; it never applies one.
  A proposed change needs the user's explicit confirmation and produces
  either (a) a one-shot override bound to this exact proposed action, used
  once: the check pauses, and the user's sealed answer to that check covers
  that one step; or (b) a new sealed version of the user's limits: the
  user's `deal note --kind approval --check <the intent's capsule_id>
  --choice confirm_limits --said "<their words>"`, which records the
  previous limits and the new ones. The agent may not activate either: seal
  either answer only when the user has said it. An intent may lower the
  limit or drop an action without a confirmation.
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

Giving anyone the user's name, phone, email, address, a pickup spot, a login or a code: typed into a page (a checkout's delivery form included) or sent in a message.

1. Note who is asking and what for, and keep the user's exact words about it.
2. Fill the form or message up to the point of sending, without sending.
3. **Final review:** `deal open` (once per deal, with the deal type the disclosure belongs to, or `service`), then `deal check` with `"action": "share_contact"` (or `"share_credentials"`), `"disclosing_to"` and `"disclosing"`: who receives it, and the class of every field you are about to give, for example `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["name","email","address"]}`. `"disclosing_to"` is required on every share check: `"counterparty"` for the deal's counterparty, `"other"` for anyone else. The check pauses the first time a class goes to this counterparty, naming them and the class, and passes with a `note` when you have told them before. When it goes to someone other than the counterparty (a courier, a platform): `"disclosing_to": "other", "recipient": {"name": "...", "phone": "..."}` with a phone, email, profile id, reply address or website. An approval covers a telling only to the party its check named.
4. Any request to the user to go ahead uses the check's `approval_text`, as it is. On `"proceed": true`, note it (step 5) and then send. On `pause`, show the card, seal the user's answer, and go on only if that returns `"proceed": true`.
5. With the form filled and **not yet sent**: `deal note --kind disclosure` naming what you are giving, by class, with the value (it stays on this device; the sealed record keeps only the class and a commitment). One disclosure per send, contact details and credentials separately:

   ```sh
   capsulectl --profile deal deal note --deal ID --kind disclosure --input d.json
   # d.json: {"channel":"sms","fields":[{"class":"phone","value":"the number you gave"},
   #                                    {"class":"pickup_location","value":"the place you gave"}]}
   ```

   `class` is `name`, `phone`, `email`, `home_address`, `address`, `pickup_location`, `other_contact`, `credential`, `verification_code`, `payment_card` or `id_document`. To someone other than the counterparty (a courier, a platform), add `"to":"other"` and `"who":{...}`. The result says whether a sealed approval covered it (`"approved"`).

   If a class would go to this counterparty for the first time and no check the user approved named it, the note seals nothing, returns `"proceed": false` with the classes `held`, and exits 7. **Do not send.** Check again with `"disclosing"` naming them, and note again once the user approves.
6. Send.
7. `deal close` when the exchange is over.

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
3. Hand the date to the host's own tracking (its goals, tasks or
   scheduler): `deal deadlines` prints every open date as JSON, and that is
   the interface. `deal deadlines --ics FILE` also writes them as a calendar
   file (with an alarm `--remind-days` before, default 2) for the user to
   import if they want. Never add anything to a calendar yourself, and never
   tell the user they will be reminded: the date is recorded; whether a
   reminder fires is up to the host. Run nothing in the background yourself.
4. Every `deal check` lists the deal's `open_deadlines`. Mention any that are
   close.
5. `deal close` refuses while a cancel-by date is open, and says why. When
   the deal is otherwise done (the order arrived, the subscription started),
   close it with `--carry-open-obligations`: the close seals the open dates,
   and they stay open after the close. `deal deadlines` and every receipt list
   each as CARRIED AT CLOSE, with its deal, until a later record resolves it
   (step 5 of "Cancellation") or the date passes, when it reads "date
   passed" with what the deal holds, as of when the listing or receipt was
   made.
6. Each obligation records one cancel-by date. A recurring renewal after that
   date is not tracked; seal a new obligation for each later date.

### Closing a deal, and evidence that arrives later

Close a deal when it reaches its point of resolution, and link anything
that arrives after that to the closed deal. Do not hold a deal open waiting
for every later email.

| Deal | Close it |
|---|---|
| A purchase of something shipped | at delivery |
| A purchase delivered at once (a download, a ticket) | at payment |
| A booking: a flight, a table, a ticket | at confirmation or ticketing |
| A booking: a stay | after the stay |
| A rental | when the item is returned |
| A service | when the work is done |

1. Every deal is opened with an expected close date (`expect_close_by`; by
   default 14 days for a purchase, 1 for a booking, 7 for a rental, 30 for a
   service). Set it when you know better, for example the delivery estimate.
2. `deal deadlines` lists every open deal under `open_deals`, against that
   date (and `--ics` writes a "Close deal …?" event for it into the file).
   Hand those to the host's tracking like any other date. When you see a
   deal there that is done, close it.
3. Evidence that arrives after the close (the merchant's confirmation, a
   shipping notice, a refund) is sealed on the closed deal as usual, as soon
   as it arrives:

   ```sh
   capsulectl --profile deal deal note --deal ID --kind evidence --email late.eml
   ```

   A closed deal takes later evidence only. It is linked to the close (it
   *confirms* the close) and commits to the closed deal, so it cannot be
   moved to another deal. Anything else needs a new deal.
4. Every receipt says whether the deal is open or closed, lists the records
   linked after the close, and says that more may be linked after it was
   made. A deal that was never closed reads "Open: no close is sealed on this
   deal", with its expected date. Pass that on as written; it is not a sign
   that anything went wrong.
5. In your own words to the user: an open deal still waiting on the
   merchant's confirmation is "waiting for the merchant's receipt"; at the
   close, say "matched" or "nothing left to match". When the merchant's email
   arrives after the close, say "The merchant's receipt arrived. It matches
   what you approved." or, when it does not, "The merchant's receipt arrived.
   It differs: you approved $4.54; their receipt says $5.20." with the real
   amounts.

### Cancellation (proving "I cancelled")

A cancel only counts as evidence when it is sealed, and it only counts as the
merchant's word when the merchant's own email says so.

1. Seal the user's words asking to cancel (`deal note --kind intent`), then
   `deal check` with `"action": "cancel"`, on the same deal that holds the
   cancel-by date. If the deal did not allow `cancel` from the start, the
   intent only proposes it and the check pauses: show the card and seal the
   user's own answer (`deal note --kind approval`). A note never adds
   `cancel` on its own.
2. On `"proceed": true`, or the user's sealed yes, cancel with the
   merchant. Right after: `deal note --kind act` with `"action": "cancel"`.
   When the cancel returns a payment, give the amount coming back
   (`"amount_minor"`, `"currency"`, `"rail"`): capsulectl records it as money
   back to the user, reversing that payment, and the receipt nets the two
   to zero instead of reading two charges.
3. When the merchant's cancellation email arrives, seal it raw:
   `deal note --kind evidence --email cancelled.eml`.
4. `deal report` then states, under "Your cancellation", exactly what is
   shown (a cancel recorded at a time, whether that was before the cancel-by
   date, and whether the merchant's own signed email confirms it) and what is
   not (that no later charge will come). Pass both lists on as written.
5. On a deal closed with the date carried, a cancel cannot be sealed as a
   step (a closed deal takes later evidence only). Seal the merchant's
   cancellation email and name the date it resolves, by its step number:

   ```sh
   capsulectl --profile deal deal note --deal ID --kind evidence --email cancelled.eml \
     --input r.json   # {"about":"the trial","source":"merchant_email","resolves_step":6}
   ```

   The date then reads RESOLVED and leaves the open list.

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
- **Note every telling, mechanically.** Whenever a page you fill or a
  message you send carries the user's name, email, phone, address, a pickup
  spot, a payment detail, a login or a code, check it first (`deal check`
  with `share_contact` or `share_credentials` and `"disclosing"`), and note
  it with `deal note --kind disclosure` once the form is filled and before
  it is sent: a first telling no approved check named is held there. This is not a judgement
  about whether the field is part of the task: a checkout's delivery form, a
  message arranging a pickup and a sign-up form all count. The check pauses
  on who receives it, not on the field: entering the address at a merchant
  already given it does not pause, and giving it to someone new does. A
  first telling with no covering approval is held, never sealed. A **repeat**
  telling (a class this party was given before) with no covering approval is
  sealed all the same and shows on the report as an agent-side anomaly;
  leaving it out only makes the record wrong.
- **Never hold silently.** A `pause` always goes to the user with the card.
- **Ask with the check's own text.** Every `deal check` returns
  `approval_text`: whether the user's rules were checked, what, who, the
  amount, how it is paid, and what the check found. When you ask the user to
  go ahead, in your own message or ahead of the host's approval card, show
  that text as it is. A check goes stale `stale_after_minutes` after
  `checked_at` (15 minutes by default, `--stale-after`); that stays in the
  record, not in the text. If the action happens after it, check again first.
- **Never say the rules ran when they did not.** When `approval_text` says
  "Your rules were not checked: …", say exactly that. Say the user's rules
  were checked only when it names them.
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

Which of your own picks (a size, a delivery option the user never named)
pause a check is set by a materiality predicate the profile pins:
`deal init --materiality FILE` or `profile update --materiality FILE` (see
`profile/materiality-predicate/`). With none configured, every attribute you
picked pauses the check and the user is asked about it. **Setting or changing
the predicate is the user's policy: never do it yourself.** If a check refuses
because the predicate changed since it was pinned, tell the user; re-pinning is
theirs to run.

Then set up the **checkpoint cadence**: schedule
`capsulectl --profile deal cll checkpoint cadence --wait-up-to 5m` **every 5
minutes** on this machine (a cron line `*/5 * * * *`, or the host's scheduler
at a five-minute interval). Each run is a poll: it waits for a tick due within
its 5 minutes and publishes it on time, on the profile's own clock (every 5m,
give or take 1m, by default), and a run with no tick due publishes nothing. A
run stays alive up to 5 minutes; if the host caps how long a scheduled job may
run, schedule more often with a matching smaller `--wait-up-to`. Never drop
`--wait-up-to`: without it a scheduled run publishes on the scheduler's grid,
which hides the cadence's jitter. Deal steps never publish anything
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
capsulectl --profile deal deal note --deal ID --kind intent   --input i.json   # {"verbatim":"the user's new words","allowed":["pay","share_contact"]}: a new action is only proposed
capsulectl --profile deal deal note --deal ID --kind disclosure --input d.json # {"fields":[{"class":"phone","value":"..."}]}
```

Seal an `intent` whenever the user changes what they asked (for example,
"go ahead and share my number"), and whenever the user picks from options
you offered (see "The user picked from options"). It replaces `verbatim` and
`asked` from then on. `allowed` and `max_total_minor` carry forward
unchanged: a higher limit or a new action in the note is only a proposal,
and the note's output says so (`proposed`, `in_force`). It applies only
when the user confirms it, as above: their answer to the paused check for
one step, or their `confirm_limits` answer to the note for a new version of
their limits. Never seal either without the user's own words.

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

For a `pay`, also state `authorized_max_minor`: **the most the payment may
take**, which is what the user's limit binds. Read it from the approval or the
card hold before you check: "the approval will ask for a maximum of $9.54" (a
$4.54 order plus up to $5 for tax settled later) means `amount_minor` 454 and
`authorized_max_minor` 954. When nothing shows a larger maximum, pass the
price: `authorized_max_minor` equal to `amount_minor`. Never pass the estimate
when a larger maximum is shown. A pay by card, wallet, PayPal, or any rail that
can hold more than it charges is refused without it.

Every check also returns `approval_text` (a short, paste-ready summary),
`checked_at` and `stale_after_minutes`, and `rules`: what the profile's rules
checker said (see "Your rules" below).

- `"verdict": "pass"`, `"proceed": true`: go ahead. Say nothing extra. The
  pass is approved by what the user already allowed, and that approval is
  sealed for you (`approval_id`).
- `"verdict": "pause"`: show `card` verbatim with its `options`, then seal
  the answer. Save the card text exactly as you showed it to a file and pass
  it with `--shown-card`: the answer then commits to that card, and the record
  shows it is the card the check produced (a different text is refused).
  `--said` is the user's own words; without them the answer is sealed as the
  card's (`agent_card`), not the user's:

```sh
capsulectl --profile deal deal note --deal ID --kind approval --check CHECK_ID --choice hold --said "the user's words" --shown-card card.txt
```

Act only if that returns `"proceed": true`. If it says details changed, or
that the check was already answered, run the check again. A check takes one
answer: to change your mind after "Hold", check again.

The card's proceed option reads "Pay anyway" (or "Confirm anyway", and so
on) only when there is a finding to override; when the check paused only
because the rules could not be checked, it reads "Pay". Show the labels as
they are.

- `"verdict": "deny"`: the user's rules do not allow this. The card names
  the rule, its limit and the value, and offers only "Hold". Do not act, and
  do not look for a way around it: tell the user what the card says.

**Your rules.** A deal profile may pin a rules checker (the user's policy,
set with `profile update --rules-checker FILE`; never yours to change). Every
check runs it on the record of what is about to happen and seals its answer:
the ruleset it ran, by id and digest, its verdict and every finding. If no
checker is configured, the check does not pause for it, and its text says
"Your rules were not checked: no rules checker configured." If a configured
checker fails, times out or was changed since it was pinned, the check pauses
and says "Your rules were not checked:" with the reason. A limit over a week
is checked against this one action alone, and the text says so: never tell
the user a weekly total was checked.

In a deal opened with `deal open --records typed`, the steps are sealed as
typed action records. A passing check is itself the authority to act (no
`approval_id`), and the user's answer to a paused check needs `--said` and
`--shown-card`. If another platform also showed the user its own approval
prompt for the same action, record what it displayed (and the user's reply, if
it returned one) as an observation. It never answers the check:

```sh
capsulectl --profile deal deal note --deal ID --kind platform_approval --check CHECK_ID --platform example-platform --mechanism native-gate --displayed-text 'Approve $558.80 purchase' --user-text 'Approve' --amount-minor 55880 --currency USD
```

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

**If DNS cannot be reached.** When the seal is refused because the key
record could not be read ("could not reach DNS for the DKIM key record …"),
do not stop. Tell the user plainly and offer the way on, in your own words,
for example: "I couldn't reach DNS to check the merchant's signature. If you
can look up the merchant's key record, I can still seal the email with it."

- **Which record.** The error names it: `SELECTOR._domainkey.DOMAIN`, a TXT
  record. `SELECTOR` and `DOMAIN` are the `s=` and `d=` tags of the email's
  `DKIM-Signature` header.
- **Where it can come from.** Any DNS lookup that can reach it: another
  resolver, or a lookup the user runs on another machine or network (for
  example `dig +short TXT SELECTOR._domainkey.DOMAIN`). Save the TXT value
  (`v=DKIM1; k=rsa; p=…`), or a zone-file line naming the record, to a file.
- **How it is sealed.** Pass the file with `--key-record`. The email is
  sealed with that record marked **supplied**: the result's `key_source` is
  `supplied`, and the report and the emailed receipt say "The merchant's key
  was supplied by hand, not read from the merchant's DNS." Say that to the
  user, too.
- **What it is worth.** A supplied record is **weaker evidence** than one this
  tool read from DNS: whoever supplied it chose it. The signature check still
  runs, but against the record you were given. Never present it as a
  resolved one.

```sh
# The seal was refused: "could not reach DNS for the DKIM key record
# s2026._domainkey.shop.example (…)". The user looked it up elsewhere:
#   dig +short TXT s2026._domainkey.shop.example  >  key-record.txt
capsulectl --profile deal deal note --deal ID --kind evidence --email confirmation.eml \
  --key-record key-record.txt
# key_source: "supplied". If the DMARC record could not be read either, pass it
# with --dmarc-record dmarc-record.txt beside --key-record (also marked supplied).
```

The result has two separate statements. Report them separately, in their own
words, and never as one combined badge:

- `merchant_says`: "merchant-confirmed: …" when the merchant's own signature
  checks out, or "not confirmed: …" with the reason and the domain's DMARC
  policy. This one is independent: it shows what the merchant sent, whatever
  the agent says. Repeat it as written. A message that is not confirmed is
  still sealed; never drop it or soften the words.
- `we_say`: what our own seal shows (this device kept these exact bytes from
  this time on). It is our own record, not the merchant's.

`parsed` holds the order number, tracking number, total, cancel-by date and items read from
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
witnessed at once: until the next tick of the profile's checkpoint cadence
(the receipt says how long at most, for example "within about 7m (every 5m,
give or take 2m)") its receipt says
"Sealed, witness pending". Steps added after a tick read "Witnessed in part"
(steps 1 to k of n witnessed, the rest pending). Say it that way to the
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
