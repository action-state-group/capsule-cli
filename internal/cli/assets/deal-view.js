// deal-view.js: the page `capsulectl deal report --html` writes. Three parts:
// what you asked, what the agent did, anomalies on either side. Each item
// expands to its sealed steps. The vendored evidence-graph verifier checks the
// whole bundle first; a step's line is shown only when the verifier matched
// the step's sealed record, otherwise the step shows its capsule_id only.
// All text is set with textContent; nothing from the bundle is parsed as HTML.
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
  const report = (bundle.extensions || {})["x-deal-v0"] || {};
  const byId = new Map((report.steps || []).map((s) => [s.capsule_id, s]));
  const base = matched.has(report.asked_step) ? bundle.disclosures[report.asked_step].agent_input : undefined;
  if (!base || !base["x-deal-v0"] || base["x-deal-v0"].record_type !== "baseline") {
    host.append(el("p", "⚠️ The deal's opening step is not in this report.", "deal-bad"));
    return;
  }

  const steps = (ids) => {
    const list = el("ol", undefined, "deal-steps");
    ids.forEach((id) => {
      const step = byId.get(id);
      const li = el("li");
      if (step && matched.has(id)) {
        li.append(el("span", `${step.at} · `, "deal-at"), el("span", step.line));
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

  if (base.body.demo) host.append(el("span", "DEMO", "deal-demo"));
  host.append(el("h1", "Deal report"));
  host.append(el("p", "This receipt covers this one deal. It is not a record of everything the agent did.", "deal-rung"));

  // The assurance rung. A witness receipt rides in checkpoint.witnesses, or
  // (the default) in the cadence chain, x-deal-cadence-v0, that anchors this
  // deal's checkpoint in the profile's cadence log. This page does not check
  // either (capsulectl verify does, against a witness directory the reader
  // chooses), and says so. Any other witness state is shown as it is.
  const cadence = (bundle.extensions || {})["x-deal-cadence-v0"] || {};
  const anchored = cadence.state === "witnessed" ? (cadence.cadence || {}).witnesses || [] : [];
  const witnesses = ((bundle.checkpoint || {}).witnesses || []).concat(anchored).filter((w) => w && typeof w.ts_url === "string");
  if (witnesses.length > 0) {
    let witness = witnesses[0].ts_url;
    try {
      witness = new URL(witness).host || witness;
    } catch (e) {
      // not a URL: show it as written
    }
    host.append(
      el("p", `Witnessed: ${witness}, an independent log, signed a receipt for this deal's checkpoint: the record existed, unchanged, by then. It does not confirm what the agent did.`, "deal-rung"),
      el(
        "p",
        "The receipt is in this file; this page does not check it. Check it with " +
          "capsulectl verify --bundle FILE --witness-directory DIRECTORY.json, using a witness directory you trust.",
        "deal-note",
      ),
    );
  } else if (cadence.state === "scheduled") {
    host.append(el("p", "Sealed by my agent. Witness: scheduled. This checkpoint goes to the witness in the next tick of the profile's cadence; it is not witnessed yet.", "deal-rung"));
  } else if (cadence.state === "pending") {
    host.append(el("p", "Sealed by my agent. Witness: pending. This checkpoint was sent in a cadence tick, but no receipt has come back yet.", "deal-rung"));
  } else {
    host.append(el("p", "Sealed by my agent: no witness receipt is in this report.", "deal-rung"));
  }
  // Written by capsulectl from what the deal recorded (dealDidLine).
  const report0 = (bundle.extensions || {})["x-deal-v0"] || {};
  if (typeof report0.did_line === "string") host.append(el("p", report0.did_line, "deal-note"));

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
  if (askedChecked) {
    host.append(item(`“${opening.text}”`, [report.asked_step]));
    host.append(el("p", "✓ These are the exact words sealed when the deal opened.", "deal-note"));
  } else {
    host.append(el("p", "⚠️ The words you asked could not be checked against the sealed record.", "deal-bad"));
  }
  host.append(
    el(
      "p",
      "The summary lines below are written by capsulectl on this device and are not checked by this page. " +
        "Open an item to see the steps it was read from; each step's record is checked.",
      "deal-note",
    ),
  );

  host.append(el("h2", "What the agent did"));
  const did = report.did || [];
  if (did.length === 0) host.append(el("p", "Nothing yet.", "deal-note"));
  did.forEach((i) => host.append(item(i.text, i.steps)));

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
        "The seal key is on the agent's machine, so this shows the record was not changed after it was made, not who made it. " +
        "It covers this skill's own records only, never the agent host's own store.",
      "deal-note",
    ),
  );
})();
