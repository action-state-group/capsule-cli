// deal-view.js: the page `capsulectl deal report --html` writes. Three parts:
// what you asked, what the agent did, anomalies on either side. Each item
// expands to its sealed steps. The vendored evidence-graph verifier checks the
// whole bundle first; a step's line is shown only when the verifier matched
// the step's sealed record, otherwise the step shows its capsule_id only.
// All text is set with textContent; nothing from the bundle is parsed as HTML.
// versionBefore reports whether version a is older than b: "v0.1.0-rc3" style,
// numbers compared as numbers, a pre-release before its release. An
// unparsable version, or a development build, is never called older.
function versionBefore(a, b) {
  const parse = (v) => {
    const m = /^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?$/.exec(v || "");
    return m ? { n: [Number(m[1]), Number(m[2]), Number(m[3])], pre: m[4] } : undefined;
  };
  const x = parse(a), y = parse(b);
  // A development build is built from source, older or newer than any tag:
  // never call it, or anything next to it, older.
  if (!x || !y || x.pre === "dev" || y.pre === "dev") return false;
  for (let i = 0; i < 3; i++) if (x.n[i] !== y.n[i]) return x.n[i] < y.n[i];
  if (x.pre === y.pre) return false;
  if (x.pre === undefined) return false;
  if (y.pre === undefined) return true;
  const num = (p) => (/^(.*?)(\d+)$/.exec(p) || [p, p, ""]);
  const [, xa, xd] = num(x.pre), [, ya, yd] = num(y.pre);
  if (xa === ya && xd !== "" && yd !== "") return Number(xd) < Number(yd);
  return x.pre < y.pre;
}

(async () => {
  const host = document.getElementById("deal");
  const bundle = window.__BUNDLE__;
  const el = (tag, text, cls) => {
    const node = document.createElement(tag);
    if (text !== undefined) node.textContent = text;
    if (cls) node.className = cls;
    return node;
  };
  let verification;
  try {
    verification = await EvidenceGraph.verifyBundle(bundle);
  } catch (e) {
    verification = undefined;
  }
  const ok =
    verification !== undefined &&
    verification.graphClosure.status === "pass" &&
    verification.intervalCoverage.status === "pass" &&
    verification.perRecordMembership.status === "pass" &&
    Object.values(verification.capsuleResults).every((r) => r.ok) &&
    verification.disclosures.every((d) => d.status === "disclosure_match" || d.status === "withheld");
  if (!ok) {
    host.append(el("p", "⚠️ This report did not verify. Do not rely on it.", "deal-bad"));
    return;
  }

  const matched = new Set(
    verification.disclosures.filter((d) => d.status === "disclosure_match" && d.member === "agent_input").map((d) => d.capsuleId),
  );
  // The deal's text: the input of the sealed report the x-deal-v0 extension
  // points at, checked like every record (its disclosure matched). A bundle
  // written before reports were sealed carries the text in the extension
  // itself, which no record seals, and the page says so.
  const dealExt = (bundle.extensions || {})["x-deal-v0"] || {};
  const sealedId = typeof dealExt.sealed_report === "string" ? dealExt.sealed_report : undefined;
  const reportInput = (id) => ((bundle.disclosures || {})[id] || {}).agent_input;
  const otherReports = verification.disclosures.filter((d) => {
    const input = d.member === "agent_input" ? reportInput(d.capsuleId) : undefined;
    return d.capsuleId !== sealedId && input && input.type === "deal_report";
  });
  // A sealed report's record stays in every later bundle (it is signed, and
  // on the log the bundle covers), so a bundle holding one whose extension
  // names none had its text swapped for unsealed text: refused, never shown
  // under the "not checked" label.
  const reportRecords = (bundle.records || []).filter((r) => r && r.action_id === "capsulectl-deal-report");
  let report = dealExt;
  if (sealedId !== undefined || otherReports.length > 0 || reportRecords.length > 0) {
    const matchedReport = verification.disclosures.some((d) => d.capsuleId === sealedId && d.member === "agent_input" && d.status === "disclosure_match");
    const input = matchedReport ? reportInput(sealedId) : undefined;
    if (otherReports.length > 0 || !input || input.type !== "deal_report" || typeof input.report !== "object" || input.report === null) {
      host.append(el("p", "⚠️ The deal's text could not be checked against its sealed record. Do not rely on it.", "deal-bad"));
      return;
    }
    report = input.report;
  }
  const sealed = sealedId !== undefined;
  const shared = report.audience === "counterparty" || report.audience === "adjudicator";
  const byId = new Map((report.steps || []).map((s) => [s.capsule_id, s]));
  const base = matched.has(report.asked_step) ? bundle.disclosures[report.asked_step].agent_input : undefined;
  if (!shared && (!base || !base["x-deal-v0"] || base["x-deal-v0"].record_type !== "baseline")) {
    host.append(el("p", "⚠️ The deal's opening step is not in this report.", "deal-bad"));
    return;
  }

  // The header, on the face of every copy: the scope, the assurance rung read
  // from what the file itself carries, what the "did" part rests on, what
  // the receipt does not claim, and the command that checks it.
  const header = el("section", undefined, "deal-assurance");
  // What the deal's text rests on, said first, on every copy: sealed with
  // the page and checked unchanged, or (a bundle written before reports were
  // sealed) read from the x-deal-v0 extension, which no record seals, so an
  // edited line would still show here.
  if (sealed) {
    const checked = el(
      "p",
      "The text on this page was written by capsulectl when the page was made and sealed as a record in this file: " +
        "this page checked that it has not changed since. It is capsulectl's summary of the steps, not the steps themselves.",
      "deal-note",
    );
    checked.dataset.sealed = "x-deal-v0";
    header.append(checked);
  } else {
    const unchecked = el(
      "p",
      "The text on this page was written by capsulectl when the page was made, and this page does not check it: " +
        "the summary lines, each step's line, the amounts and what was told. " +
        "This page checks the sealed records in this file, not those words." +
        (shared ? "" : " Only the words you asked, marked ✓, are checked against their sealed record."),
      "deal-note",
    );
    unchecked.dataset.unchecked = "x-deal-v0";
    header.append(unchecked);
  }
  header.append(el("p", report.scope || "This receipt covers this one deal. It is not a record of everything the agent did.", "deal-scope"));
  // A witness receipt rides in checkpoint.witnesses, or (the default) in the
  // cadence chain, x-cadence-witness/v0 (earlier bundles: cadence-witness/v0
  // or x-deal-cadence-v0), that anchors this deal's checkpoint in
  // the profile's cadence log. This page does not check either (capsulectl
  // verify does, against a witness directory the reader chooses), and says
  // so. Any other witness state is shown as it is.
  const exts = bundle.extensions || {};
  const cadence = exts["x-cadence-witness/v0"] || exts["cadence-witness/v0"] || exts["x-deal-cadence-v0"] || {};
  const anchored = cadence.state === "witnessed" ? (cadence.cadence || {}).witnesses || [] : [];
  const witnesses = ((bundle.checkpoint || {}).witnesses || []).concat(anchored).filter((w) => w && typeof w.ts_url === "string");
  if (witnesses.length > 0) {
    let witness = witnesses[0].ts_url;
    try {
      witness = new URL(witness).host || witness;
    } catch (e) {
      // not a URL: show it as written
    }
    const cut = typeof cadence.checkpoint_at === "string" && cadence.checkpoint_at ? `cut at ${cadence.checkpoint_at} ` : "";
    // An earlier checkpoint that already held every step (only sealed
    // reports after it) witnesses them all.
    const part = cadence.state === "witnessed" && cadence.extent === "part" && Number(cadence.steps_witnessed) < Number(cadence.steps);
    // What those steps hold, in the user's terms, as capsulectl named them.
    const wc = report.witness_coverage || {};
    const coverage =
      (typeof wc.witnessed_acts === "string" && wc.witnessed_acts ? `Witnessed, steps 1 to ${Number(cadence.steps_witnessed)}: ${wc.witnessed_acts}. ` : "") +
      (typeof wc.pending_acts === "string" && wc.pending_acts ? `Witness pending, steps ${Number(cadence.steps_witnessed) + 1} to ${Number(cadence.steps)}: ${wc.pending_acts}. ` : "");
    const k = Number(cadence.steps_witnessed);
    const n = Number(cadence.steps);
    header.append(
      el(
        "p",
        part
          ? `Witnessed in part: ${witness}, an independent log, signed a receipt for this deal's checkpoint, ${cut}at a cadence tick, covering steps 1 to ${k} of ${n}: those existed, unchanged, by then. Steps ${k + 1} to ${n} are sealed by my agent on this device only, witness pending. ${coverage}It does not confirm what the agent did.`
          : `Witnessed: ${witness}, an independent log, signed a receipt for this deal's checkpoint, ${cut}at a cadence tick after the deal's steps: the record existed, unchanged, by then. It does not confirm what the agent did.`,
        "deal-rung",
      ),
      el(
        "p",
        "The receipt is in this file; this page does not check it. Check it with " +
          "capsulectl verify --bundle FILE --witness-directory DIRECTORY.json, using a witness directory you trust.",
        "deal-note",
      ),
    );
  } else if (cadence.state === "scheduled") {
    const every = typeof cadence.cadence === "string" && cadence.cadence ? ` (${cadence.cadence})` : "";
    header.append(el("p", `Sealed by my agent, witness pending. A deal is not witnessed at the moment it happens: its checkpoint goes to the witness at the next tick of this profile's cadence${every}, and only then can it be witnessed. Until then it is sealed on this device only.`, "deal-rung"));
  } else if (cadence.state === "pending") {
    header.append(el("p", "Sealed by my agent, witness pending. This checkpoint was sent at a cadence tick, but no receipt has come back yet; it is retried at every tick.", "deal-rung"));
    if (cadence.reason === "network_consent_needed" && typeof cadence.text === "string") header.append(el("p", `Witness ${cadence.text}`, "deal-note"));
  } else {
    header.append(el("p", "Sealed by my agent: no witness receipt is in this report.", "deal-rung"));
  }
  // The countersign rung, as capsulectl checked it when it wrote this page
  // (a page cannot resolve a signer against a directory): "Not
  // countersigned" unless the bundle carried a countersignature that
  // verified, and a self-countersignature is NOT INDEPENDENT. A file that
  // carries a countersignature capsulectl did not check says only that.
  // The page renders the line capsulectl wrote; it names no rung itself.
  let countersign = { text: "The countersign line could not be read from this file." };
  const csNode = document.getElementById("deal-countersign");
  if (csNode) {
    try {
      const parsed = JSON.parse(csNode.textContent);
      if (parsed && typeof parsed.text === "string") countersign = parsed;
    } catch (e) {
      // unreadable: say so, and claim nothing
    }
  }
  header.append(el("p", countersign.text, countersign.flag === true ? "deal-rung deal-bad" : "deal-rung"));
  if (typeof countersign.note === "string" && countersign.note) header.append(el("p", countersign.note, "deal-note"));
  // Written by capsulectl from what the deal recorded (dealDidLine): the
  // conversation is rightly the agent's own record; what the agent did needs
  // an independent source.
  if (typeof report.did_line === "string") header.append(el("p", report.did_line, "deal-note"));
  const notClaimed = el("details", undefined, "deal-claims");
  notClaimed.open = true;
  notClaimed.append(el("summary", "What this does not claim"));
  const claims = el("ul");
  [
    "It is tamper-evident, not non-repudiation: it shows these records were not changed after they were sealed, not who made them.",
    "It records what the agent reported. It does not show that what was reported was true.",
    "It does not prove the merchant shipped, delivered or refunded anything.",
  ].forEach((t) => claims.append(el("li", t)));
  notClaimed.append(claims);
  header.append(notClaimed);
  // A second opinion without trusting this page or installing anything: a
  // verifier that checks a dropped file in the reader's own browser.
  header.append(el("p", "This page checked itself. If you would rather not take its word, open verify.agentactioncapsule.org and drop this file in.", "deal-note"));
  header.append(el("p", "Anyone can check this file offline, with only the file and capsulectl:", "deal-note"));
  header.append(el("pre", report.verify_command || "capsulectl verify --bundle receipt.html", "deal-verify"));
  if (shared) {
    const who = report.audience === "counterparty" ? "the other party" : "an adjudicator";
    header.append(el("p", `A shared copy for ${who}. Left out of this copy: ${(report.withheld || []).join(", ")}.`, "deal-note"));
  }

  const recordOk = (id) => verification.capsuleResults[id] !== undefined && verification.capsuleResults[id].ok;
  const steps = (ids) => {
    const list = el("ol", undefined, "deal-steps");
    ids.forEach((id) => {
      const step = byId.get(id);
      const li = el("li");
      if (step && matched.has(id)) {
        li.append(el("span", `${step.at} · `, "deal-at"), el("span", step.line));
      } else if (shared && step && step.withheld && recordOk(id)) {
        // A withheld step: its record and its place in the log verified;
        // its contents are not in this copy.
        li.append(el("span", `${step.at} · `, "deal-at"), el("span", step.line), el("span", " · contents withheld", "deal-at"));
      } else {
        li.append(el("span", "step not verified in this report "), el("code", id));
      }
      list.append(li);
    });
    return list;
  };
  const item = (text, ids, cls) => {
    const d = el("details", undefined, cls);
    d.append(el("summary", text), steps(ids));
    return d;
  };

  if (base && base.body.demo) host.append(el("span", "DEMO", "deal-demo"));
  host.append(el("h1", "Deal report"));

  // Which build sealed the steps, read from the records this page verified
  // (not from the summary), compared with the build that made this page.
  // Both are already in this file: nothing is fetched to do it.
  const provenance = el("section", undefined, "deal-provenance");
  const builds = [];
  matched.forEach((id) => {
    const rec = ((bundle.disclosures || {})[id] || {}).agent_input || {};
    // An x-deal-v0 record names its producer in its block; a typed record, in its header.
    const p = (rec["x-deal-v0"] || rec).producer;
    const name = p && typeof p.version === "string" ? `${p.name || "capsulectl"} ${p.version} (${p.commit || "unknown"})` : "an earlier capsulectl that did not record its version";
    if (!builds.some((b) => b.name === name)) builds.push({ name, version: p && p.version });
  });
  if (builds.length > 0) provenance.append(el("p", `Produced by ${builds.map((b) => b.name).join(", then ")}.`, "deal-note"));
  if (typeof report.instructions === "string" && report.instructions) provenance.append(el("p", report.instructions, "deal-note"));
  const pageVersion = typeof report.page_version === "string" ? report.page_version : "";
  const older = builds.filter((b) => b.version === undefined || versionBefore(b.version, pageVersion));
  if (pageVersion && older.length > 0) {
    const what = el("p", `Produced by an older version (${older.map((b) => b.version || "unrecorded").join(", ")}) than the capsulectl that made this page (${pageVersion}). What changed: `, "deal-note");
    const link = document.createElement("a");
    link.href = "https://github.com/action-state-group/capsule-cli/releases";
    link.textContent = "the release notes";
    link.rel = "noopener";
    what.append(link);
    provenance.append(what);
  }

  // The user's words are checked here against the baseline's sealed
  // commitment: SHA-256 over JCS({"nonce","text"}). For two string members in
  // this order, JSON.stringify escapes exactly as RFC 8785 does.
  const opening = report.asked_opening || {};
  let askedChecked = false;
  if (typeof opening.nonce === "string" && typeof opening.text === "string") {
    const jcs = `{"nonce":${JSON.stringify(opening.nonce)},"text":${JSON.stringify(opening.text)}}`;
    const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(jcs));
    const hex = Array.from(new Uint8Array(digest), (x) => x.toString(16).padStart(2, "0")).join("");
    askedChecked = hex === base.body.intent.verbatim_commitment;
  }
  host.append(el("h2", "What you asked"));
  if (shared) {
    host.append(el("p", "Withheld from this copy.", "deal-note"));
  } else if (askedChecked) {
    host.append(item(`“${opening.text}”`, [report.asked_step]));
    host.append(el("p", "✓ These are the exact words sealed when the deal opened.", "deal-note"));
  } else {
    host.append(el("p", "⚠️ The words you asked could not be checked against the sealed record.", "deal-bad"));
  }
  host.append(
    el(
      "p",
      sealed
        ? "The summary lines below, and each step's line, were written by capsulectl on this device and sealed with this page: " +
            "this page checked they were not changed since, not that they are right. Open an item to see the steps it was read from; each step's sealed record is checked."
        : "The summary lines below, and each step's line, are written by capsulectl on this device and are not checked by this page. " +
            "Open an item to see the steps it was read from; each step's sealed record is checked, not its line.",
      "deal-note",
    ),
  );

  host.append(el("h2", "What the agent did"));
  const did = report.did || [];
  if (did.length === 0) host.append(el("p", "Nothing yet.", "deal-note"));
  did.forEach((i) => host.append(item(typeof i.at === "string" && i.at ? `${i.at} · ${i.text}` : i.text, i.steps)));
  // What the sealed acts moved, by direction: a pay and its reversal net to zero.
  if (report.money && typeof report.money.text === "string") host.append(el("p", report.money.text));

  // Authority: for each action, every layer it relied on, in order, each with
  // its time; never collapsed into "you approved". Written by capsulectl on
  // this device; each line expands to the step it was read from.
  const authority = report.authority || [];
  if (authority.length > 0) {
    host.append(el("h2", "Authority"));
    if (typeof report.authority_order === "string") host.append(el("p", report.authority_order, "deal-note"));
    authority.forEach((b) => {
      host.append(el("h3", `${b.covered ? "" : "⚠️ "}${b.action}`));
      (b.layers || []).forEach((l) => {
        const line = item(`${l.at} · ${l.text}`, [l.step], b.covered || l.layer !== "action" ? undefined : "deal-flag");
        if (typeof l.note === "string" && l.note) line.append(el("p", l.note, "deal-note"));
        host.append(line);
      });
    });
  }

  // Where the deal stands (open, cancelled or closed), the records linked to
  // the close after it, and that more may be linked after this page was
  // made: a fact, before what the page can and cannot prove about it.
  // Written by capsulectl when the page was made ("as of"); each linked
  // record expands to its sealed step.
  const life = report.lifecycle;
  if (life && typeof life.text === "string") {
    host.append(el("h2", "Where this deal stands"), el("p", life.text));
    (life.later || []).forEach((l) => host.append(item(`${l.at} · confirms the close: ${l.text}`, [l.capsule_id])));
    host.append(el("p", `${life.may_change} (as of ${life.as_of})`, "deal-note"));
  }

  // What this receipt can prove and how to check it, after the facts it is about.
  host.append(header, provenance);

  // What the agent told whom about the user: each telling, its time and the
  // approval that covered it, or that none did. Your own copy shows what was
  // given; a shared copy names only the kind of thing.
  host.append(el("h2", "What your agent told whom"));
  const told = report.told || [];
  if (told.length === 0) host.append(el("p", "Nothing about you was recorded as shared.", "deal-note"));
  told.forEach((t) => {
    const flagged = t.authority === "none";
    const fields = (t.fields || []).map((f) => (typeof f.value === "string" ? `${f.label}: ${f.value}` : f.label)).join("; ");
    host.append(item(`${flagged ? "⚠️ " : ""}${t.text} · ${t.at} · ${t.authority_text}${shared ? "" : ` (${fields})`}`, t.steps, flagged ? "deal-flag" : undefined));
  });
  host.append(el("p", "Only what the agent sealed as shared is listed here; something it told without sealing it is not.", "deal-note"));

  // Where the user sells: what their agent told the buyer, each claim's words
  // checked here against the commitment its sealed step carries, labelled by
  // the class sealed with it. A claim that is not the agent's own, or whose
  // words do not match, is never shown as said.
  const said = report.representations || [];
  if (said.length > 0) {
    const labels = {
      price: "Price", condition: "Condition", features: "Features", authenticity: "Authenticity",
      availability: "Availability", delivery_date: "Delivery date", service_scope: "Service scope",
      warranty: "Warranty", refund_terms: "Refund terms", payment_methods: "Payment methods",
      pickup: "Pickup", deadline: "Deadline", address: "Address", other: "Other",
      delivery_promise: "Delivery promise", // a claim sealed before delivery_date
    };
    const heading = !shared ? "What your agent told the buyer" : report.audience === "counterparty" ? "What the seller's agent told you" : "What the seller's agent told the buyer";
    host.append(el("h2", heading));
    for (const r of said) {
      const rec = matched.has(r.step) ? ((bundle.disclosures || {})[r.step] || {}).agent_input : undefined;
      const body = rec && rec.body;
      const claim = body && (Number.isInteger(r.index) ? (body.claims || [])[r.index] : body);
      let ok = false;
      if (claim && claim.source_kind === "agent" && typeof r.nonce === "string" && typeof r.text === "string") {
        const jcs = `{"nonce":${JSON.stringify(r.nonce)},"text":${JSON.stringify(r.text)}}`;
        const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(jcs));
        ok = Array.from(new Uint8Array(digest), (x) => x.toString(16).padStart(2, "0")).join("") === claim.text_commitment;
      }
      if (ok) {
        const label = labels[claim.class];
        host.append(item(`${label ? `${label}: ` : ""}“${r.text}” ✓`, [r.step]));
      } else {
        host.append(item("⚠️ A statement here could not be checked against its sealed step.", [r.step], "deal-flag"));
      }
    }
    host.append(el("p", "✓ These are the exact words sealed when the agent said them; this page does not check that they are true.", "deal-note"));
  }

  // Cancel-by dates (the point of no return is a date passing) and due
  // dates (what the user owes by a date). Recorded, never enforced.
  const deadlines = report.deadlines || [];
  if (deadlines.length > 0) {
    host.append(el("h2", deadlines.some((d) => d.due_by) ? "Cancel-by and due dates" : "Cancel-by dates"));
    const list = el("ul", undefined, "deal-steps");
    deadlines.forEach((d) => {
      const live = d.status === "open" || d.status === "carried_at_close";
      const li = el("li", undefined, live ? "deal-bad" : undefined);
      li.append(el("span", `${d.cancel_by || d.due_by} · ${d.marking || d.status}: `), el("span", `${d.text}. ${d.holds || ""}`));
      list.append(li);
    });
    host.append(list, el("p", deadlines[0].note, "deal-note"));
  }
  // A cancellation: exactly what is shown, and what is not.
  (report.cancellations || []).forEach((c) => {
    host.append(el("h2", "Your cancellation"));
    const shows = el("details", undefined, c.merchant === "confirmed" ? undefined : "deal-flag");
    shows.open = true;
    shows.append(el("summary", "What this shows"));
    const proven = el("ul");
    (c.proven || []).forEach((p) => proven.append(el("li", p)));
    const notProven = el("ul", undefined, "deal-note");
    (c.not_proven || []).forEach((p) => notProven.append(el("li", p)));
    shows.append(proven, el("h3", "What this does not show"), notProven, steps(c.steps || []));
    host.append(shows);
  });

  // The merchant's own emails. Two statements, never one: the merchant's
  // DKIM signature (independent of the agent) and our seal (our own record).
  const merchant = report.merchant || [];
  if (merchant.length > 0) {
    host.append(el("h2", "The merchant's own email"));
    host.append(
      el(
        "p",
        "Two different things stand behind each email. The merchant's signature is checked against the merchant's published key, " +
          "saved when the email was sealed; it shows what the merchant sent, and it does not depend on the agent. " +
          "Our seal shows this device kept these exact bytes from that time on; it is our own record. " +
          "Amounts, dates and order numbers below are read from the email by capsulectl and may be misread.",
        "deal-note",
      ),
    );
    if (typeof report.email_scope === "string") host.append(el("p", report.email_scope, "deal-scope"));
    merchant.forEach((m) => {
      const d = el("details", undefined, m.verified ? undefined : "deal-flag");
      d.open = true;
      d.append(el("summary", m.order_id ? `Order ${m.order_id}` : "Merchant email"));
      d.append(el("p", `Merchant's signature: ${m.merchant_says}`, m.verified ? "deal-ok" : "deal-bad"));
      d.append(el("p", `Our seal: ${m.we_say}`, "deal-note"));
      if (m.key_source === "supplied") {
        d.append(el("p", "The merchant's key was supplied by hand, not read from the merchant's DNS.", "deal-bad"));
      }
      const table = el("table", undefined, "deal-merchant");
      const row = (label, value) => {
        if (!value) return;
        const tr = el("tr");
        tr.append(el("th", label), el("td", value));
        table.append(tr);
      };
      row(m.approved_basis ? `You approved (${m.approved_basis})` : "You approved", m.approved);
      row("The agent reported paying", m.agent_reported);
      row("The merchant's email says (read from the email)", m.charged);
      row("Charged on (the email's date)", m.charged_on);
      row("Cancel by (read from the email)", m.cancel_by);
      row("Items (read from the email)", (m.items || []).join(" · "));
      row("Signing domain", m.domains);
      row("Merchant's key", m.key_size ? `${m.key_size}, sealed when the email was sealed` : "");
      d.append(table, steps(m.steps || []));
      host.append(d);
    });
  }

  host.append(el("h2", "Anomalies"));
  const anomalies = report.anomalies || [];
  [
    ["agent", "Agent side"],
    ["counterparty", "Counterparty side"],
  ].forEach(([side, label]) => {
    const mine = anomalies.filter((a) => a.side === side);
    host.append(el("h3", label));
    if (mine.length === 0) host.append(el("p", "None found.", "deal-note"));
    mine.forEach((a) => host.append(item(`⚠️ ${a.text}`, a.steps, "deal-flag")));
  });

  host.append(
    el(
      "p",
      "This page checked itself: every step's sealed record and its place in this deal's log. " +
        "The records carry fingerprints, not names or numbers; the words shown come from this device. " +
        (shared ? "Steps marked contents withheld are proven present and unchanged; what they say is not in this copy. " : "") +
        "The seal key is on the agent's machine, so this shows the record was not changed after it was made, not who made it. " +
        "It covers this skill's own records only, never the agent host's own store.",
      "deal-note",
    ),
  );
})();
