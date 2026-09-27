// deal-view.js: the deal section of a `capsulectl deal report --html` page.
// It runs after the vendored evidence-graph verifier and shows a deal step's
// content only when that verifier matched the content against the step's
// sealed digest. Withheld steps show their capsule_id only. All text is set
// with textContent; nothing from the bundle is parsed as HTML.
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
    host.append(el("p", "⚠️ This receipt did not verify. Do not rely on it.", "deal-bad"));
    return;
  }

  // Only content the verifier matched against its seal is shown.
  const matched = new Set(
    verification.disclosures.filter((d) => d.status === "disclosure_match" && d.member === "agent_input").map((d) => d.capsuleId),
  );
  const ext = (bundle.extensions || {})["x-deal-v0"] || {};
  const steps = (ext.steps || []).map((s) => {
    const shown = matched.has(s.capsule_id);
    const body = shown ? bundle.disclosures[s.capsule_id].agent_input : undefined;
    return { id: s.capsule_id, n: shown ? body.n : s.n, kind: shown ? body.kind : s.kind, at: shown ? body.at : "", body };
  });
  const open = steps.length > 0 && steps[0].body && steps[0].body.open;
  if (!open) {
    host.append(el("p", "⚠️ The deal's opening step is not in this receipt.", "deal-bad"));
    return;
  }

  const symbols = { USD: "$", EUR: "€", GBP: "£", CAD: "CA$", AUD: "A$" };
  const money = (minor, cur) => {
    if (typeof minor !== "number") return "";
    const c = (cur || "").toUpperCase();
    const s = `${Math.floor(Math.abs(minor) / 100)}.${String(Math.abs(minor) % 100).padStart(2, "0")}`;
    return (minor < 0 ? "-" : "") + (symbols[c] ? symbols[c] + s : c ? `${s} ${c}` : s);
  };
  const describe = (who, terms, recourse, currency) => {
    const parts = [];
    if (who) {
      const w = [who.payee && `pay ${who.payee}`, who.name, who.domain, who.phone, who.email, who.relay_address, who.profile_id].filter(Boolean);
      if (w.length) parts.push(w.join(", "));
    }
    if (terms) {
      const cur = terms.currency || currency;
      const t = [
        terms.quantity && `${terms.quantity} ×`,
        terms.item,
        terms.price_minor !== undefined && `price ${money(terms.price_minor, cur)}`,
        terms.deposit_minor !== undefined && `deposit ${money(terms.deposit_minor, cur)}`,
        terms.when,
        terms.place,
      ].filter(Boolean);
      if (t.length) parts.push(t.join(" "));
    }
    if (recourse && recourse.rail) {
      parts.push(`by ${recourse.rail}${recourse.refundable === false ? ", not refundable" : recourse.refundable ? ", refundable" : ""}`);
    }
    return parts.join(" · ");
  };
  const row = (table, label, value) => {
    if (!value) return;
    const tr = el("tr");
    tr.append(el("th", label), el("td", value));
    table.append(tr);
  };

  const currency = open.terms.currency;
  const closed = steps.filter((s) => s.body && s.body.close).pop();
  const head = el("header");
  if (open.demo) head.append(el("span", "DEMO", "deal-demo"));
  head.append(el("h1", "Deal receipt"));
  head.append(el("p", `You asked: “${open.intent.verbatim}”`, "deal-ask"));
  head.append(el("p", `Outcome: ${closed ? closed.body.close.outcome : "open"}`));
  host.append(head);
  host.append(
    el(
      "p",
      "Checked in this page: every step's seal, its place in this deal's log, and every detail shown against its seal. " +
        "The log is signed by a key on the agent's machine, so this shows the record was not changed after it was made. " +
        "It does not prove who made it.",
      "deal-note",
    ),
  );

  // One block per judged action: asked, agreed, about to happen, differences,
  // the user's answer, and what was done. Flagged ones start expanded.
  const judged = el("section");
  judged.append(el("h2", "Checks before each point of no return"));
  const byId = new Map(steps.map((s) => [s.id, s]));
  const acts = steps.filter((s) => s.body && s.body.act);
  const claimedActs = new Set();
  steps
    .filter((s) => s.body && s.body.check)
    .forEach((s) => {
      const check = s.body.check;
      const snap = byId.get(check.snapshot);
      const answer = steps.find((a) => a.body && a.body.approval && a.body.approval.check === s.id);
      const act = acts.find((a) => a.body.n > s.body.n && a.body.act.action === check.action && !claimedActs.has(a.id));
      if (act) claimedActs.add(act.id);
      const flagged = check.verdict === "pause" || (act && act.body.act.unchecked);
      const d = el("details");
      d.open = flagged;
      d.append(el("summary", `${flagged ? "⚠️ " : "✓ "}${check.action}: ${check.verdict === "pass" ? "no differences" : "paused"}`));
      const table = el("table");
      row(table, "You asked", open.intent.verbatim);
      row(table, "Agreed", describe(open.who, open.terms, open.recourse, currency));
      const sb = snap && snap.body && snap.body.snapshot;
      if (sb) {
        row(table, "About to happen", [sb.description, sb.amount_minor !== undefined && money(sb.amount_minor, currency), describe(sb.who, sb.terms, sb.recourse, currency)].filter(Boolean).join(" · "));
      }
      row(table, "Differences", (check.differences || []).map((x) => x.text).join(" · ") || "none");
      if (check.unverified && check.unverified.length) row(table, "Unverified", check.unverified.join(", "));
      if (answer) row(table, "Your answer", answer.body.approval.choice + (answer.body.approval.said ? ` (“${answer.body.approval.said}”)` : ""));
      if (act) {
        const a = act.body.act;
        row(table, "What was done", `${a.action}${a.amount_minor !== undefined ? " " + money(a.amount_minor, a.currency || currency) : ""}${a.payee ? " to " + a.payee : ""}${a.rail ? " by " + a.rail : ""}` + (a.unchecked ? ` · ⚠️ skipped check: ${a.reason}` : " · as checked"));
      }
      d.append(table);
      judged.append(d);
    });
  acts
    .filter((a) => !claimedActs.has(a.id))
    .forEach((a) => {
      const d = el("details");
      d.open = true;
      d.append(el("summary", `⚠️ ${a.body.act.action}: done with no check`));
      const table = el("table");
      row(table, "You asked", open.intent.verbatim);
      row(table, "What was done", `${a.body.act.action}${a.body.act.amount_minor !== undefined ? " " + money(a.body.act.amount_minor, a.body.act.currency || currency) : ""}${a.body.act.payee ? " to " + a.body.act.payee : ""}`);
      row(table, "Skipped check", a.body.act.reason);
      d.append(table);
      judged.append(d);
    });
  host.append(judged);

  const trail = el("details");
  trail.append(el("summary", `Every step (${steps.length})`));
  const list = el("ol");
  steps.forEach((s) => {
    const li = el("li");
    li.append(el("span", `${s.at ? s.at + " · " : ""}${s.kind}`));
    li.append(el("code", s.body ? "" : ` withheld · ${s.id}`));
    list.append(li);
  });
  trail.append(list);
  host.append(trail);

  // The library's own verification model, drawn from the verifier result.
  const model = EvidenceGraph.buildVerificationPageModel(bundle, verification);
  const checks = el("details");
  checks.append(el("summary", "How this receipt was checked"));
  const facts = el("table");
  row(facts, "Receipt digest", model.bundleDigest || "uncomputable");
  row(facts, "Log checkpoint", model.checkpointRoot ? `${model.checkpointRoot} (size ${model.checkpointSize})` : "absent");
  if (model.selfWitnessed) row(facts, "Witness", "self-witnessed: no transparency-service receipt");
  model.receipts.forEach((r) => row(facts, "Witness", `${r.witness} · ${r.grade} · ${r.time}`));
  checks.append(facts);
  const ol = el("ol");
  model.checks.forEach((c) => ol.append(el("li", `${c.name}: ${c.result}`)));
  checks.append(ol, el("p", model.verifyIndependentlyLine, "deal-note"));
  host.append(checks);
})();
