// deal-view.js: the page `capsulectl deal report --html` writes. Three parts:
// what you asked, what the agent did, anomalies on either side. Each item
// expands to its sealed steps. The vendored evidence-graph verifier checks the
// whole bundle first; a step's content is shown only when the verifier matched
// it against the step's seal, otherwise the step shows its capsule_id only.
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
  const kinds = new Map((report.steps || []).map((s) => [s.capsule_id, s.kind]));
  const body = (id) => (matched.has(id) ? bundle.disclosures[id].agent_input : undefined);
  const open = body(report.asked_step);
  if (!open || !open.open) {
    host.append(el("p", "⚠️ The deal's opening step is not in this report.", "deal-bad"));
    return;
  }
  const currency = open.open.terms.currency;
  const symbols = { USD: "$", EUR: "€", GBP: "£", CAD: "CA$", AUD: "A$" };
  const money = (minor, cur) => {
    if (typeof minor !== "number") return "";
    const c = (cur || currency || "").toUpperCase();
    const s = `${Math.floor(Math.abs(minor) / 100)}.${String(Math.abs(minor) % 100).padStart(2, "0")}`;
    return (minor < 0 ? "-" : "") + (symbols[c] ? symbols[c] + s : c ? `${s} ${c}` : s);
  };
  const who = (w) => (w ? [w.payee && `pay ${w.payee}`, w.name, w.domain, w.phone, w.email, w.relay_address, w.profile_id].filter(Boolean).join(", ") : "");
  const terms = (t) =>
    t
      ? [t.quantity && `${t.quantity} ×`, t.item, t.price_minor !== undefined && `price ${money(t.price_minor, t.currency)}`, t.deposit_minor !== undefined && `deposit ${money(t.deposit_minor, t.currency)}`, t.when, t.place]
          .filter(Boolean)
          .join(" ")
      : "";
  const recourse = (r) => (r && r.rail ? `by ${r.rail}${r.refundable === false ? ", not refundable" : r.refundable ? ", refundable" : ""}` : "");
  const join = (...parts) => parts.filter(Boolean).join(" · ");

  // One line of plain words per sealed step.
  const stepText = (b) => {
    switch (b.kind) {
      case "open":
        return join(`Opened: “${b.open.intent.verbatim}”`, who(b.open.who), terms(b.open.terms), recourse(b.open.recourse));
      case "message":
        return `${b.message.from}${b.message.channel ? ` (${b.message.channel})` : ""}: “${b.message.text}”`;
      case "claim":
        return `Claim (${b.claim.source}): ${b.claim.text}${b.claim.verified ? " · checked" : ""}`;
      case "evidence":
        return join(`Evidence (${b.evidence.source}): ${b.evidence.about}`, b.evidence.detail, b.evidence.verified ? "checked" : "");
      case "change":
        return join(`Changed (${b.change.source})`, who(b.change.who), terms(b.change.terms), recourse(b.change.recourse));
      case "snapshot":
        return join(`About to ${b.snapshot.action}`, b.snapshot.description, b.snapshot.amount_minor !== undefined && money(b.snapshot.amount_minor), who(b.snapshot.who), terms(b.snapshot.terms), recourse(b.snapshot.recourse));
      case "check":
        return b.check.verdict === "pass" ? "Check: no differences" : `Check paused: ${b.check.differences.map((d) => d.text).join(" · ")}`;
      case "approval":
        return `Your answer: ${b.approval.choice}${b.approval.said ? ` (“${b.approval.said}”)` : ""}${b.approval.reason ? ` · ${b.approval.reason}` : ""}`;
      case "act":
        return join(`Done: ${b.act.action}`, b.act.amount_minor !== undefined && money(b.act.amount_minor, b.act.currency), b.act.payee && `to ${b.act.payee}`, b.act.rail && `by ${b.act.rail}`, b.act.unchecked ? `⚠️ ${b.act.reason}` : "as checked");
      case "close":
        return join(`Closed: ${b.close.outcome}`, ...(b.close.differences || []).map((d) => d.text));
      default:
        return b.kind;
    }
  };
  const steps = (ids) => {
    const list = el("ol", undefined, "deal-steps");
    ids.forEach((id) => {
      const b = body(id);
      const li = el("li");
      if (b) {
        li.append(el("span", `${b.at} · `, "deal-at"), el("span", stepText(b)));
      } else {
        li.append(el("span", `${kinds.get(id) || "step"} (content withheld) `), el("code", id));
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

  if (open.open.demo) host.append(el("span", "DEMO", "deal-demo"));
  host.append(el("h1", "Deal report"));

  host.append(el("h2", "What you asked"));
  host.append(item(`“${open.open.intent.verbatim}”`, [report.asked_step]));

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
      "This page checked itself: every step's seal and its place in this deal's log. " +
        "The seal key is on the agent's machine, so this shows the record was not changed after it was made, not who made it.",
      "deal-note",
    ),
  );
})();
