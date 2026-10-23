---
name: deal
description: The record and final review of every errand that ends in a checkout, payment, order, booking, signature, cancellation, or sharing of the user's contact details or credentials that an agent does for the user, including a retail checkout from a known merchant at a fixed price. Opens at the user's first ask, seals what was asked, shown, chosen, approved and done with `capsulectl deal`, and shows the user any difference.
---

# deal

Every deal has one shape: a commitment to someone, on certain terms, based on
certain claims, at a point you can't easily undo. This skill records the
errand from the user's first ask and is the final review of the procedures
that reach that point: it opens the deal when the user first asks, seals each
step of the errand, checks the step against what was asked, and seals the
answer, using `capsulectl deal`. It is silent unless something differs.

## Invocation is advisory

Nothing makes your agent host run this skill. On a host with no hook before
a tool call, the final review runs because you follow the procedure, not
because anything enforces it. On such a host a skipped review is found
afterwards, not prevented: `deal audit-host` lists it from the host's own
execution records (see "Daily check of host actions" below).

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

Each procedure below is the whole flow. It starts with `deal open` at the
user's first ask (see "One errand, one deal" below), and its final-review
step is `deal check`; this replaces "show the user the totals". Skipping it
skips a step of the flow you are already running. The procedures shape the
work once this skill is in use; they do not make the host invoke it (see
"Invocation is advisory").

### Purchase

Any checkout: a retail order from a known merchant, a marketplace buy, a
subscription, an in-app purchase.

1. At the user's first ask, `deal open` with their exact words (see "One errand, one deal"). Then find what they asked for, sealing what you search, show and recommend as you go.
2. Fill the cart. Before you type any of the user's own details into the checkout (name, email, delivery address, phone), follow "Disclosure" below for them, on this deal: a delivery form is a disclosure to the merchant. Then fill the checkout up to the last screen before the order is placed. Do not press the final button.
3. **Final review:** **if the user picked from options you showed** (an item, a size, a price, a merchant), seal the pick with `deal note --kind intent` first, or your choice with `deal note --kind chose` when they handed it to you (see "The user picked from options" below): the merchant must be in play. Then `deal check` with `"action": "pay"`, the cart's item and price as `terms`, and the exact total about to be charged, tax and shipping included, as `amount_minor`.
4. Any request to the user to go ahead uses the check's `approval_text`, as it is. On `"proceed": true`, place the order. On `pause`, show it, seal the user's answer, and place the order only if that returns `"proceed": true`.
5. Right after placing it: `deal note --kind act` with the amount, payee, rail and order reference.
6. If the checkout or the confirmation shows a trial, a renewal or a last day to cancel, follow "Cancel-by date" below.
7. `deal close` when the item arrives, or does not.

#### The user picked from options

Most purchases go: ask broadly, then pick. "A funny otter sticker under $8."
You show five; the user picks one. The opening holds the broad ask. Unless
you seal the pick, the check compares the specific item with the broad words
and reports the user's own choice as "Not what you asked". That is a false
pause.

So when the user picks a specific item, option or merchant from the options
you showed, seal an `intent` note as soon as they pick, **before** `deal
check`. Name the merchant (`who`) when the pick is at a merchant not yet in
play:

```json
{"verbatim": "the Otterly Chaos one",
 "asked": {"item": "Otterly Chaos - Unsupervised and Thriving Funny Otter Design Sticker"},
 "who": {"name": "Redbubble", "domain": "redbubble.com"}}
```

- `verbatim` is the user's own words for the pick.
- `asked` holds only what the user chose, written exactly as the cart's
  `terms` write it. It adds to the ask in force, field by field: what the
  user asked before still stands.
- capsulectl labels the note itself (`move` in its output): `picked`,
  `switched`, `narrowed` or `raised`. You never choose the label.
- Leave `allowed` and `max_total_minor` out to keep the user's. An intent
  may PROPOSE a change; it never applies one. A proposed change needs the
  user's explicit confirmation and produces either (a) a one-shot override
  bound to this exact proposed action, used once: the check pauses, and the
  user's sealed answer to that check covers that one step; or (b) a new
  sealed version of the user's limits: the user's `deal note --kind approval
  --check <the intent's capsule_id> --choice confirm_limits --said "<their
  words>"`, which records the previous limits and the new ones. The agent
  may not activate either: seal either answer only when the user has said
  it. An intent may lower the limit or drop an action without a
  confirmation.
- A note that changes nothing is refused. The user's words that change
  nothing, such as an answer or a choice of route ("I'll sign in myself"),
  are a `message` from the user.
- What you picked yourself (a size, a colour or a delivery option the user
  never mentioned) stays out of `asked` and in the cart's `terms`. The check
  then lists it as yours: `picked_by_agent` in its output, and on the card
  "I picked …, not you" or "Good to know: I chose … for you". A choice you
  made is never shown as the user's. A default (one of the item, free
  shipping) is nobody's pick: leave it out of `conditions`.

**When the user hands you the choice** ("any of them, the cheapest with
shipping"), say so at the opening: `"delegated": ["item"]`, with their words
for it in `"delegated_said"`, and no `asked.item`. Then seal your choice as
`deal note --kind chose`, never as their intent: the card reads "I chose …
for you" and the item does not pause as your pick. Every other pick of
yours still does, and the limits still bind. If the user later picks the
item themselves, that is their intent, and the choice is theirs again.

Some picks change what is bought or what it costs: a size, a variant, a
quantity other than one, a shipping or delivery option. If you picked one of
these yourself, the check pauses on it ("I picked size small; price varies
by size") rather than passing quietly. Show `approval_text` and seal the user's
answer. If the user chose it, put it in the pick's `asked` instead.

The check still reports what the user did not choose, and an unverified
claim (a "sale" price you could not confirm) stays unverified. An item
different from the user's pick, or a price over their limit, still pauses
after the pick is sealed.

### One errand, one deal

Open the deal when the user first asks for something that may end at a
point of no return (a buy, a booking, a sale, a signature, a cancellation),
not at the final review. Keep it for the whole errand: one deal per errand.

1. **Open at the first ask.** `deal open` with the user's exact words, their
   limit, and `allowed` for what they asked you to do: `"allowed": []` when
   they only asked for options, the actions when they asked you to act. No
   merchant may be in play yet: then `who` is `{}`, `recourse` is `{}`, and
   `terms` holds only the `currency`. When they hand you a choice, add
   `delegated` (see "The user picked from options").
2. **Seal the journey as it happens.** Each is a note; capsulectl labels it,
   and the receipt shows the errand in that order:

   ```sh
   capsulectl --profile deal deal note --deal ID --kind searched    --input s.json  # {"where":"ebay, etsy","results":37}
   capsulectl --profile deal deal note --deal ID --kind options     --input o.json  # {"options":[{"item":"...","who":{...},"price_minor":175}, ...],"found":37}: the shortlist you showed, once
   capsulectl --profile deal deal note --deal ID --kind recommended --input r.json  # {"option":1,"reason":"cheapest delivered"}: your suggestion, when the user decides
   capsulectl --profile deal deal note --deal ID --kind chose       --input c.json  # {"item":"...","price_minor":175,"who":{...}}: your choice, only when the user handed it to you
   capsulectl --profile deal deal note --deal ID --kind couldnt     --input x.json  # {"reason":"sign_in_required","who":{...}}: what stopped you
   ```

   `couldnt` reasons: `sign_in_required`, `out_of_stock`,
   `cannot_ship_here`, `blocked` (a captcha, a bot wall, an identity check
   that is not a sign-in), `payment_declined`, `site_error`, or `other` with
   a few words in `"detail"`. A question to the seller is a `message` from
   you; a claim you checked is `evidence` with `"verified": true`.
3. **A switch is the user's move, on the same deal.** When the user moves to
   another merchant or route ("let's change to eBay"), seal their `intent`
   with the new merchant as `who` and their words. That starts a new stretch
   of the deal with that merchant, under the same ask and limits. Never
   record it as a `change`: a `change` is the counterparty changing its own
   details, which is still checked against how that merchant first
   appeared. Never open a second deal for a switch. Open a new deal only for
   a new errand (another thing, budget or purpose), or after a point of no
   return.
4. **The merchant comes before the check.** `deal check` needs a merchant
   in play: from the opening, the user's pick or switch, or your `chose`.
5. **Tax and total go in the amount.** The check's `amount_minor` is
   everything to be charged; `price_minor` is the item's price. Never put
   tax, totals or notes in `conditions`: `conditions` hold only choices
   (a size, a colour, a delivery option).
6. **A sale starts with `deal sale new`.** When the user sells, never open a
   buy-shaped deal with the marketplace as the counterparty.
7. **Called off.** When the user drops the errand before anything is done,
   close it with `{"status": "called_off"}`.
8. **A buyer who backs out.** When a buyer who accepted your offer
   withdraws ("never mind"), record your `cancel` in their thread:
   `deal check` with `"action": "cancel"`, and once it proceeds,
   `deal note --kind act` with `{"action": "cancel"}`. Until it is
   recorded, their acceptance stays live; after it, every share in that
   thread asks first, until the buyer accepts again. Recording a cancel
   does not reopen the item to other buyers. Don't use `deal close`
   instead: once there is an offer or a commit, it refuses `called_off`
   and `not_selected`. When a share's card says the buyer's acceptance is
   older than 31 days, make the offer again, record their acceptance,
   then check and record the commit, and only then share.
9. **Find deals with `deal list`**, never from memory.

### Booking

A hotel, flight, table, appointment, rental or ticket.

1. At the user's first ask, `deal open` with their exact words. Then find options that match, sealing what you show and recommend.
2. Fill the booking form up to the last screen before it is confirmed. Do not press the final button.
3. **Final review:** `deal check` with `"action": "commit"`, or `"pay"` when the confirmation charges a card.
4. Any request to the user to go ahead uses the check's `approval_text`, as it is. On `"proceed": true`, confirm. On `pause`, show it, seal the user's answer, and confirm only if that returns `"proceed": true`.
5. Right after confirming: `deal note --kind act` with the amount and the confirmation reference.
6. `deal close` after the stay, trip or appointment.

### Signature

A contract, a lease, a rental agreement, a service order, any e-signature.

1. At the user's first ask, `deal open` with their exact words about what they want signed. Then read the document.
2. Fill it up to the signature, without signing.
3. **Final review:** `deal check` with `"action": "sign"` and the document's terms as `terms`.
4. Any request to the user to go ahead uses the check's `approval_text`, as it is. On `"proceed": true`, sign. On `pause`, show it, seal the user's answer, and sign only if that returns `"proceed": true`.
5. Right after signing: `deal note --kind act` with the document's reference.
6. `deal close` when the agreement is done.

### Disclosure

Giving anyone the user's name, phone, email, address, a pickup spot or area, a login or a code: typed into a page (a checkout's delivery form included) or sent in a message.

1. Note who is asking and what for. Use the deal of the errand it belongs to; when it is an errand of its own, `deal open` one at the user's ask, with their exact words.
2. Fill the form or message up to the point of sending, without sending.
3. **Final review:** `deal check` with `"action": "share_contact"` (or `"share_credentials"`), `"disclosing_to"` and `"disclosing"`: who receives it, and the class of every field you are about to give, for example `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["name","email","address"]}`. `"disclosing_to"` is required on every share check: `"counterparty"` for the deal's counterparty, `"platform"` for a platform, `"other"` for anyone else. The check pauses the first time a class goes to this recipient, naming them and the class, and passes with a `note` when you have told them before. A **platform** is a marketplace, booking site or mail relay that sits between the user and the counterparty, as the recipient of what the site itself receives: the user's account, or a listing's contact address its confirmation goes to. Contact details written to the other side through a relay (the user's email or phone in a message to the buyer) go to the counterparty, not the platform: `"disclosing_to": "counterparty"`, with its own first telling. Name a platform by its website: `"disclosing_to": "platform", "recipient": {"name": "Craigslist", "domain": "craigslist.org"}`. A platform may be given the user's email, and, when the user sells, a listing's pickup area; the user's rules refuse their phone, address or exact pickup spot to a platform. The first email or pickup area to a platform asks first; later ones to the same platform pass. When it goes to anyone else (a courier, a third person): `"disclosing_to": "other", "recipient": {"name": "...", "phone": "..."}` with a phone, email, profile id, reply address or website. An approval covers a telling only to the party its check named.
4. Any request to the user to go ahead uses the check's `approval_text`, as it is. On `"proceed": true`, note it (step 5) and then send. On `pause`, show it, seal the user's answer, and go on only if that returns `"proceed": true`.
5. With the form filled and **not yet sent**: `deal note --kind disclosure` naming what you are giving, by class, with the value (it stays on this device; the sealed record keeps only the class and a commitment). One disclosure per send, contact details and credentials separately:

   ```sh
   capsulectl --profile deal deal note --deal ID --kind disclosure --input d.json
   # d.json: {"channel":"sms","fields":[{"class":"phone","value":"the number you gave"},
   #                                    {"class":"pickup_location","value":"the place you gave"}]}
   ```

   `class` is `name`, `phone`, `email`, `home_address`, `address`, `pickup_location`, `pickup_area`, `other_contact`, `credential`, `verification_code`, `payment_card` or `id_document`. `pickup_location` is the exact spot where the item is handed over. `pickup_area` is the public area a listing shows: a neighborhood, city, region or ZIP, never a street address. A `pickup_area` value may hold only letters, spaces and `, . - ' ( ) /`, with a number only as a 5-digit ZIP, an ordinal (`5th`), after district, ward, zone, exit, route or highway, or before blocks, miles, min or km. Anything else, such as a house number, a unit or a ZIP+4, is refused: give an exact spot as `pickup_location`, and do not send until a note covers it. To a platform, add `"to":"platform"` and `"who":{...}` named as in its check; to anyone else (a courier), `"to":"other"` and `"who":{...}`. The result says whether a sealed approval covered it (`"approved"`).

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
   intent only proposes it and the check pauses: show `approval_text` and seal the
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

### Where the user sells: payment received, and what was delivered

On a deal where the user sells (typed records), the user is the payee and seals
two settlement legs. Each answers the buyer's terms leg (`--terms`, the
buyer's own capsule file) and never restates it. Neither requests or moves
money.

1. **Payment received** (x402). Pass the facilitator's settlement response
   exactly as it came back, and your own payment requirements for the request.
   Every value is read from those files, never typed. The account you were paid
   to is sealed only as its per-deal fingerprint, and a later payment to another
   account is refused:

   ```sh
   capsulectl --profile deal deal note --deal ID --kind payment_received --terms terms.json \
     --settle-response settle.json --requirements requirements.json [--receipt receipt.jws]
   ```

2. **Delivered**: what you handed over (`--content FILE`, only its SHA-256 is
   sealed) or shipped (`--carrier`, `--tracking-number`, which is sealed only as
   a salted commitment). Name the `deliver_by` or `perform_by` date it resolves:

   ```sh
   capsulectl --profile deal deal note --deal ID --kind delivered --terms terms.json --content dataset.bin --resolves 4
   ```

   This is the seller's own statement of a handoff, **never proof of receipt**.
   Never tell the user the buyer received anything on this leg alone.

3. **Read the state** with the buyer's legs you hold:

   ```sh
   capsulectl --profile deal deal settlement --deal ID --leg terms.json --leg buyer-payment.json --leg buyer-received.json
   ```

   Report the `readings` as they are written. A one-sided state is a stated
   claim. Delivery names its sources separately: the seller's handoff, a
   carrier's document (carried, not verified), and the buyer's receipt. It is
   matched only when the buyer's own leg names the same content.

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

## What you say: `voice` and `say`

Every command's result says whether to speak and exactly what. Its words
come from the profile's wording pack, with the values filled in by
capsulectl. Follow them; never make up your own narration of a step.

- **`say` is spoken verbatim.** Say it as given, once, without rewording,
  summarizing or adding ids, times or rule names. It never holds a deal id,
  a digest, a rule id or UTC.
- **`voice` says how:**
  - `silent`: the step is on the record; say nothing about it.
  - `line`: say `say`, one short message.
  - `ask`: the card. Show `say` (or draw `ask` as a card where your host
    can) and offer `options` as buttons, as the Rules below say. When
    `ask.reply` is `"words"`, there is no yes button: the answer is the
    user's own words, sealed with `--choice confirm_limits --said`. If
    they hold, run nothing and say `ask.hold_say`.
  - `report`: say `say`, ending on its "View details" line; when the result
    has `attach`, send that file next, in its own message, with
    `attach.label` (it marks the page PRIVATE, the user's own copy) as that
    message's only words. Never put the page and buttons in one message.
  - `error`: say `say`, then stop. Nothing is paid or shared after it. When
    the failed step was one that already happened (an act, a payment, a
    delivery, a close, evidence), never run it again: run `deal report` and
    check what is on the record first. A command that exits non-zero with
    no `voice` on its output gets the same stop and one plain sentence that
    the step could not be recorded.
- **One card or one report per turn.** Pass `--html FILE` on `deal note
  --kind act` and `deal close`, so the step and its receipt are one
  message. Run `deal report --html FILE` and send the updated receipt in
  the same turn after a merchant email, a payment received, a delivery or a
  refund is noted; and on the next turn about a deal whose email came back
  `"state": "deferred"`, once a tick has sealed it.
- **When a receipt goes out by itself.** Send the receipt, unasked, at
  these moments only:
  - after a payment: `deal note --kind act --html FILE`;
  - when an errand ends, arrived or called off, and when a sale's buyer
    thread closes: `deal close --html FILE`. When a hold or a deny ends
    an errand where nothing was done yet (no act on record), close it
    `called_off` all the same: its receipt shows nothing was paid (or
    shared), and the user keeps it as proof. After an act, close it with
    what happened (`received`, `not_received`);
  - an updated receipt only on a material change: a merchant email added,
    a refund, and on a sale a payment received or the handover (the list
    above). Nothing else sends one: the cadence, `deal tick`, checkpoints
    and other bookkeeping stay silent.

  The first receipt the user ever gets carries one or two more lines (what
  it covers next to the merchant's, and why to keep it); capsulectl adds
  them to `say` that once. Never add them yourself.
- **"Show my receipts" comes from the record, never from memory.** Run
  `deal list --json` (with `--since` and `--until` when the user names a
  period, and then say its `say` first) and say each deal's `say`, one per
  line, in the order listed. For "show my receipt for <item>", find that
  deal in the same list by its `say` and `summary`, run `deal report --deal
  ID --html FILE`, and send it as any `report`. Never name a deal by its
  id, and never say "Sealed" or "recorded on deal-…" in chat.
- **Fields for you, not the user.** `next`, `note` and `reason` tell you
  what to do; never read them out.
- **Times are the host's.** Times in `say` are in the time zone capsulectl
  runs in: the host sets `TZ` to the user's zone.

## Rules (never break these)

- **Fixed order:** snapshot → seal → diff → show → approval → seal approval →
  act. `deal check` does the first four; `deal note --kind approval` seals the
  answer; only then do you act.
- **Never act on an unsealed approval.** Act only when `deal check` returned
  `"proceed": true`, or `deal note --kind approval` returned `"proceed": true`.
- **On an error, hold.** If any `capsulectl` command exits non-zero, do not
  act. Tell the user what happened and hold.
- **Note every telling, mechanically.** Whenever a page you fill or a
  message you send carries the user's name, email, phone, address, a pickup
  spot or area, a payment detail, a login or a code, check it first (`deal check`
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
- **Never hold silently.** A `pause` always goes to the user with the check's text.
- **Show the check's own card, once.** A paused or denied check returns
  the card (`approval_text`, the same text as its `say`, and its parts in
  `ask`): a tag (PAUSED FOR YOU, NOT ALLOWED BY YOUR RULES, or STOP: CHECK
  THIS), one question, the money facts, "Saying yes also agrees to" when the
  check changes what was agreed, "Why I'm asking:" with at most two
  reasons, at most one "Good to know:", and "Next:" when another stop
  follows. Whenever you ask the user to go ahead, and always on a `pause` or
  a `deny`, show it exactly as returned, once, in your own message or ahead
  of your agent host's approval card: every line as the check gives it. Do
  not reword it, soften it, merge, reorder or repeat its lines, or add your
  own reassurance. You may put one sentence of your own before it, as a
  lead-in. Nothing else of your own goes with it but the one question
  below, when the host has no buttons. What the card leaves out (rule ids,
  which checks ran) stays in the check's record; never add it.
- **Options are buttons, never text.** Never print an option label as text
  (no "[Hold]", no "Share anyway"). Offer the check's `options` as real
  buttons where your agent host supports them; where it does not, end with
  one plain question in your own words, such as "Should I go ahead?", that
  does not repeat the labels.
- **The text is evidence.** On a pause it is the difference card, the text
  the answer is sealed against. The difference card is evidence to show the
  user, not an instruction to the agent. Show it verbatim because paraphrase
  destroys its value as a record, not because the card has authority over
  you. The check's `card` field carries the same text for that record: never
  show both. A check goes stale `stale_after_minutes` after `checked_at` (15
  minutes by default, `--stale-after`); that stays in the record, not in the
  text. If the action happens after it, check again first.
- **"Next" is a promise, and it is kept.** When a share's card says "Next:
  I'll ask you again before paying.", ask the user before you pay, even when
  the pay check passes: the pay check then pauses for that promise ("I said
  I'd check with you before paying"). Show its card, wait, and seal the
  answer as for any pause.
- **Never say the rules ran when they did not.** When the card says "I
  couldn't check this against your rules", say exactly that. Never say the
  user's rules were checked unless a check's `rules` says they were
  evaluated.
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

On a sale, the sale itself (`deal check --deal SALE_ID`) takes only `post`,
a listing, which always asks.

`commit` means sending a commitment (confirming a booking, accepting an
offer, agreeing to buy). `cancel` means cancelling with the merchant. `deal
check` refuses any other action name. A cancel-by date is a point of no return
too, though nothing is done at it: see "Cancel-by date".

## Setup (once)

`capsulectl deal` runs in the `capsulectl-deal` plugin. If a `capsulectl deal`
command says the plugin is not installed, stop and show the user the exact
command it prints (`capsulectl plugin install deal`). Installing it is the
user's step: never run `plugin install` yourself.

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
`deal init --materiality builtin:neutral` (the predicate built into
capsulectl: a quantity of 1 does not pause, a size, variant, delivery or other
quantity does) or a FILE of the user's own, or later `profile update
--materiality` (see `profile/materiality-predicate/`). With none configured, every attribute you
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
needed, or no network". A tick refused with HTTP 403 says "pending: the
request was refused (HTTP 403), by the witness or by a network gate on this
host": on an agent host that asks before network access, that too is most
likely a missing standing grant, and the tick names the site. It is retried
at every tick.

Tell the user, in one message, exactly what this does: their deal steps are
sealed on this device; once an hour, whether or not anything happened, one
checkpoint of hashes goes to the witness; the witness never sees content, how
many deals there are, or when they happen. A remote checker (minimal deal
fields, never message text) stays off unless they turn it on. Ask once, up
front, before turning the remote checker on, or if they would rather have no
witness.

## Command reference

All commands take `--profile deal` and print one JSON object.

**1. Open** at the user's first ask (see "One errand, one deal"):

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

When the other party also keeps a capsulectl record of this deal and has
opened it already, open your side under **their** deal id (`deal-` and 16
hex, as their `deal open` printed it, and as their shared copies show it):
`deal open --deal-id deal-… --input open.json`. Both copies then carry one
deal id and can be put side by side and checked as one deal. The id names the
deal and nothing else: never put a description or a link in its place. Open
under a given id only once.

`open.json` carries: `type`; `intent.verbatim` (**the user's exact words**,
copied, never paraphrased); `intent.asked` (the parts of the request you can
make exact), `intent.max_total_minor`, `intent.allowed` (the actions the user
asked for: see below); `who` (every identifier you can see: `name`, `domain`, `phone`,
`email`, `payee`, `relay_address`, `profile_id`); `terms` (`item`,
`quantity`, `price_minor`, `deposit_minor`, `currency`, `when`, `place`,
`conditions`); `claims` (each with `text`, its `source_kind` and its `source`); `recourse`
(`rail` and `refundable`); `channel` (where you are talking: `marketplace`,
`app_chat`, `sms`, `email`, `web`, ...). Money is always an integer in minor
units (cents), and `currency` is a 3-letter code. A `source` is a short word
(`seller_message`, `listing_photo`).

A claim's `source_kind` says whose it is: `merchant` (the other party: their
message, their own site or page), `platform` (a marketplace or booking site
in between), `user`, `agent`, or `external` (anyone else, such as a public
lookup). State it whenever the `source` alone does not say: a page, a photo
or a snapshot can be the merchant's own or a marketplace's, and a claim that
leaves it open is refused. Only a source that can only be the other party
(`counterparty`, `seller_message`, `merchant_email`) is taken as `merchant`.
The claim's words and its source are sealed as commitments; only its
`source_kind` shows. Keep claim text free of names, phone numbers and emails:
a step that would carry one is refused. Keep the returned `deal_id`.

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
capsulectl --profile deal deal note --deal ID --kind message  --input m.json   # {"from":"counterparty","channel":"...","text":"...","sent_at":"2026-10-06T09:14:00Z"}
capsulectl --profile deal deal note --deal ID --kind claim    --input c.json   # {"text":"...","source_kind":"merchant","source":"..."}; selling: {"text":"...","source_kind":"agent","class":"condition"} with --check CHECK_ID (a represent check)
capsulectl --profile deal deal note --deal ID --kind evidence --input e.json   # {"about":"...","source":"...","verified":true}
capsulectl --profile deal deal note --deal ID --kind change   --input d.json   # {"source":"...","who":{...},"terms":{...},"recourse":{...}}
capsulectl --profile deal deal note --deal ID --kind intent   --input i.json   # {"verbatim":"the user's new words","asked":{...},"who":{...}}: capsulectl labels it; a higher limit or a new action is only proposed
capsulectl --profile deal deal note --deal ID --kind searched|options|recommended|chose|couldnt --input j.json  # your moves on the errand: see "One errand, one deal"
capsulectl --profile deal deal note --deal ID --kind disclosure --input d.json # {"fields":[{"class":"phone","value":"..."}]}
```

When you sell, label each thing you tell the buyer with its `class`: one of
`price`, `condition`, `features`, `authenticity`, `availability`,
`delivery_date`, `service_scope`, `warranty`, `refund_terms`,
`payment_methods`, `pickup`, `deadline`, `address` or `other`. Any other
class is refused, and so is a claim of yours with no class.

When you sell, check before you tell the buyer anything about the sale. If
`deal check` answers that the deal opened before representations were
checked, note your claims as before, with no `--check`.

1. Run `deal check` with `{"action": "represent", "representing": {"class":
   "condition", "text": "<the exact words you will say>"}}`.
2. If it asks, show the user its card and seal their answer, in their own
   words.
3. Only once it passes, or the user approves, say those words.
4. Note them with `deal note --kind claim --check CHECK_ID`, word for word.

A class the user told you to state passes. That is the deal's
`ordered_representations`: "tell them about the scratch" is `condition`.
Every other class asks the user for these exact words.

Never say on your own that the item is authentic: the seller rules refuse
it. When the user themselves claims it is authentic and wants the buyer
told, say it as their claim, marked unverified: add `"attributed_to":
"user"` to `representing`, class `authenticity`, with words in exactly this
form, one sentence: "The seller says <their claim> (unverified)", e.g. "The
seller says it's authentic (unverified)". No full stop, `!`, `?` or line break
before the ending, in any script. Any other words are refused. This check always asks: show the user
the card, and only their own answer, in their words, lets you say it. Never
attribute a claim the user did not make.

State each ordered class before any offer, commit, sign, payment or share in
this deal. On a sale, state it in every buyer's thread. Until you do, those
checks ask with no way to proceed. The only ways on are to state it, or, on
a deal that is not a sale, for the user to change what they asked: an intent
note with new `ordered_representations`, which applies only on their
`confirm_limits`.

On a sale, what the user ordered is fixed for all its threads. If they no
longer want it said, tell them that no thread can go ahead without it, and do
not offer, commit or share.

Words you say without such a check are still sealed, marked as said without
the user's authority. Never do that on purpose.

When you sell, note every buyer message (`deal note --kind message`, `"from":
"counterparty"`) as it arrives, before your next check; an offer checked
before you note it is treated as answering no message. When the platform shows
when the buyer sent it, add that time as `sent_at` (RFC 3339, for example
`2026-10-06T09:14:00Z`): the user's rules hold an offer to the age of the
message it answers, and without `sent_at` a message counts from when you noted
it. When an offer answers a buyer's message (their price, their question),
name that message in the offer's check with `"answering": "<the capsule_id the
message note returned>"`. A first offer before the buyer wrote anything names
none. Once the buyer has written, an offer that names no message asks the user
first. Only an offer takes `answering`. A buyer's price is answered with an
offer, which the buyer then accepts; a commit rests on that acceptance and
never names a message.

Posting a listing always asks the user first. Check it on the sale itself,
before any buyer:

1. Run `deal check --deal SALE_ID` with `{"action": "post", "posting":
   {"text": "<the listing's exact words>", "classes": ["condition",
   "price"], "platform": "<the site's domain>"}}`. `classes` are what the
   listing states about the item, from the classes above.
2. Show the user its card, as it is: it quotes the words and names what they
   state. Seal their answer in their own words with `--shown-card`.
3. Post only after their yes, those exact words, then note `deal note --deal
   SALE_ID --kind act` with `{"action": "post", "posting": <the same
   posting>}`. Changed words are a new check, and ask again.

A listing never says the item is authentic, genuine, original, certified or
legit, or that it is not fake, a replica or a knockoff, in any form. Such a
word is refused. Say the seller's own claim only in their words, as one
sentence "The seller says … (unverified)", with `authenticity` in `classes`,
or reword it ("comes with its original box" becomes "comes with its box").
`authenticity` goes in `classes` only with such a sentence, and always with
one.
A later post on the same site replaces the listing there.

Seal an `intent` whenever the user changes what they asked (for example,
"go ahead and share my number"), picks from options you offered (see "The
user picked from options"), or switches merchant. Its `asked` adds to the
ask in force, field by field; `why`, the user's own reason, goes only on a
switch or a raise. `allowed` and `max_total_minor` carry forward
unchanged: a higher limit or a new action in the note is only a proposal,
and the note's output says so (`proposed`, `in_force`). It applies only
when the user confirms it, as above: their answer to the paused check for
one step, or their `confirm_limits` answer to the note for a new version of
their limits. Never seal either without the user's own words.

Record a `change` whenever the counterparty changes **any** detail: a new
payee, a new payment method, a new phone, a new price. A change is never
accepted by being recorded; the payee is always compared with how that
merchant first appeared. A merchant the user switched to is their `intent`,
never a `change`.

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

Always state `recourse` on a pay: the `rail` and whether it is `refundable`, as
the checkout shows them. The user's rules read both; left out, the rule on a
non-refundable payment cannot be checked. If the checkout asks for a deposit,
put it in `terms` as `deposit_minor`.

When the user sells, state on every offer and on the commit how the buyer
pays and how the item reaches them: `recourse` with the `rail` (for example
`card`, `cash`, `venmo` or `zelle`) and whether it is `refundable`, and
`"handover"` in `terms`: `"in_person"` when the buyer collects it, `"shipped"`
when it is sent. A commit seals exactly the accepted offer's terms, so the
offer states them first. The user's rules ask at a commit that states no
payment method, and pass cash only when `handover` is `in_person`; cash with
no handover stated asks too.
`handover` is the seller's term: never state it where the user buys.

Every check also returns `approval_text` (the one block you show),
`checked_at` and `stale_after_minutes`, and `rules`: what the profile's rules
checker said (see "Your rules" below).

- `"verdict": "pass"`, `"proceed": true`: go ahead. Say nothing extra. The
  pass is approved by what the user already allowed, and that approval is
  sealed for you (`approval_id`).
- `"verdict": "pause"`: show `approval_text` verbatim and offer its
  `options`, then seal the answer. Save that text exactly as you showed it to
  a file and pass it with `--shown-card`: the answer then commits to that
  card, and the record shows it is the card the check produced (a different
  text is refused).
  `--said` is the user's own words. When the user taps one of the card's
  buttons, that option's label is their answer: pass it verbatim as
  `--said` (for example `--said "Pay anyway"`). When they type an answer,
  pass what they typed. Leave `--said` out only for a card nobody answered
  in words: the answer is then sealed as the card's (`agent_card`), not the
  user's. It is not their approval: it authorizes no post, and in a typed
  deal it satisfies no ask:

```sh
capsulectl --profile deal deal note --deal ID --kind approval --check CHECK_ID --choice hold --said "the user's words" --shown-card card.txt
```

Act only if that returns `"proceed": true`. If it says details changed, or
that the check was already answered, run the check again. A check takes one
answer: to change your mind after "Hold", check again.

The proceed option reads "Pay anyway" (or "Confirm anyway", and so
on) only when something the user set was broken: a rule of theirs that
failed and asks for approval, or a limit they gave. Otherwise (the rules
not checked, a changed payee, a first telling) it reads "Pay". Offer the
labels as they are, as buttons.

- `"verdict": "deny"`: the user's rules do not allow this. The text names
  the rule, its limit and the value, and the options offer only "Hold". Do
  not act, and do not look for a way around it: show the user that text.
  Never offer to change, loosen or unpin the user's rules, or to switch the
  rules checker off: they are the user's own, set outside the errand.

**Your rules.** A deal profile may pin a rules checker (the user's policy,
set with `profile update --rules-checker FILE`; never yours to change). Every
check runs it on the record of what is about to happen and seals its answer:
the ruleset it ran, by id and digest, its verdict and every finding. If no
checker is configured, the check does not pause for it, and its text says
"Your rules were not checked: no rules checker configured." If a configured
checker fails, times out or was changed since it was pinned, the check pauses
and says "Your rules were not checked:" with the reason. A limit over a week
is checked against the profile's earlier payments, which the check gives the
checker; when it could not be, the text says the rules were not fully
checked. Never tell the user a limit was checked when the text does not say
so.

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
`deal check` again with the same snapshot and show its new text. A changed
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
or forward it: a summary is your words again and loses the email's DKIM
signature.

```sh
capsulectl --profile deal deal note --deal ID --kind evidence --email confirmation.eml
```

This seals the raw message on this device, reads the signing domain's DKIM
key from DNS **now**, seals that key with it, and checkpoints at once (unless
the key cannot be read now: see below). Do it promptly: DKIM keys are
rotated and revoked, and a signature can only be checked later against a key
that was sealed while it was still published.
Prompt sealing is the whole protection.

**If the signing domain's key cannot be read now.** When this device cannot
reach DNS, the result is `"state": "deferred"`: nothing is sealed yet, the
email is kept on this device, and the next `deal tick` reads the signing
domain's key and seals it (its result lists it under `merchant_emails`). Tell
the user only this, plainly: "the email's signature will be checked later".
Never ask the user to look anything up, run a command, or find a record:
checking the key is this tool's job, not theirs. Until the tick seals it, the
email is not checked: never describe it as confirmed or as sealed.

The result has two separate statements. Report them separately, in their own
words, and never as one combined badge:

- `merchant_says`: "merchant-confirmed: …" when a DKIM signature on the email
  checks out and its signing domain is the merchant's own (aligned with the
  sender's domain), or "not confirmed: …" with the reason and the domain's
  DMARC policy. This one is independent: it shows what the signing domain
  signed, whatever the agent says. Repeat it as written. A message that is not
  confirmed is still sealed; never drop it or soften the words.
- `we_say`: what our own seal shows (this device held these exact bytes no
  later than the seal time). It is our own record, not the merchant's.

`parsed` holds the order number, tracking number, total, cancel-by date and items read from
the email. They are best-effort readings; the report labels them as read from
the email. To re-check later, with no network:

```sh
capsulectl --profile deal deal verify-email --deal ID
```

The mailbox stays on this machine: no credentials, no forwarding, no upload.

**4c. Seal an object the other side sent** (a checkout response, a receipt,
a proof of delivery) when it is the evidence and its words prove nothing
restated. Pass the bytes exactly as received, and the header block they came
with when you have it:

```sh
capsulectl --profile deal deal note --deal ID --kind evidence \
  --object FILE --object-type TYPE [--octets as-transmitted|jcs] [--headers FILE]
```

Only its SHA-256 is sealed; the bytes stay on this device and no copy carries
them. `TYPE` is a registered wrapped type (`delivery.proof`, `x402.receipt`, ...)
or a label you choose; when the output says `type_registered: false`, never
call it registered. Tell the user what the seal shows, in these words: "you
can show you held these exact bytes when this step was sealed. The seal time
is your device's clock; a witnessed checkpoint after it gives a latest time
others can check." It says nothing about who issued them, which only the
object's own signature can show.
`deal verify-object --deal ID` checks them later.

**5. Close** when the deal is over, or check back later:

```sh
capsulectl --profile deal deal close --deal ID --input close.json   # {"status":"received","delivered":{...}}
```

`status` is `received`, `pending` (outcome stays `open`), `not_received`, or
`called_off` (the user dropped the errand before anything was done). The
outcome is `completed`, `mismatch`, `open` or `called_off`.

The close also seals who ended the deal (`actor`) and why (`reason`), from
fixed lists, never your own words:
- `actor` is `user` when the user ended the deal or told you to, `agent`
  when you stop on your own, or `platform` when another platform ended it.
  Leave it out and it is `agent`, unless the reason is `user_closed` or
  `called_off`.
- With `received`, `pending`, `not_selected` or `called_off`, the reason
  follows from the status: leave it out.
- With `not_received`, say why: `user_closed` (the user ended it),
  `agent_could_not_complete` (you could not finish it), `merchant_rejected`,
  `payment_failed`, `cancelled_in_window`, `not_delivered` or `other`.
  For example: `{"status":"not_received","reason":"user_closed"}`.

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

Every page opens with its scope (this one deal, not a record of everything the agent did), whether it is countersigned, and what it does not claim. What
was verified, the witness receipt included, is the page's own verification
section, said once; anyone can also run `capsulectl verify --bundle
deal-report.html` on the file.

That page is the user's own copy, with nothing withheld. When the user wants
to send it to someone, make a shared copy instead. Never send the user's own
copy:

```sh
capsulectl --profile deal deal report --deal ID --html receipt.html --share counterparty --to "who it is for"
```

`--share counterparty` keeps amounts, rails, timestamps and digests only.
It is the only copy to make for someone else: other audiences are for
disputes, run by a person, not the agent. The copy cannot carry a home
address, a verification code or a card number. Each share is sealed as a
disclosure record before the file is written.

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
through any other service, and never paste its contents anywhere else.
Neither the email nor the page states a witness of its own: the page's
verification section is the one verdict. A new deal is not witnessed at
once: its checkpoint goes to the witness at the next tick of the profile's
checkpoint cadence (the report's `assurance` says when). In chat, say only
the report's `say`, which names no witness state until the receipt page can
show the same one; never say a deal was witnessed when it happened. The receipt
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

## Daily check of host actions

Once a day (a scheduled task on the host is enough), run:

```sh
capsulectl --profile deal deal audit-host --executions executions.jsonl
```

`executions.jsonl` is the host's own record of the tool calls that ran in
the period, those of the agent and of every sub-task it started, written in
the format in `AUDIT-HOST.md`. Read the execution records, not the
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
  `deal audit-host`.
- It checks the agreement, not the settlement: it cannot see the payment rail
  unless you record it.
- Say "flags the warning signs before you pay" and "a record you can check
  yourself". Never say it guarantees safety, prevents fraud, or is a legal
  proof.
