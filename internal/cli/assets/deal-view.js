// deal-view.js: the deal page's presentation module, capsulectl.deal-view/v0,
// for the page `capsulectl deal report --html` writes. Three parts: what you
// asked, what the agent did, anomalies on either side. Each item expands to its
// sealed steps.
//
// It is a module-slot script (agent-action-capsule's presentation contract):
// it registers its module with the page's runtime at load, and the runtime's
// resolver selects it for a verified deal bundle. It never verifies and never
// reads the bundle's records or disclosures itself: buildModel reads the
// verified context the runtime built, and what capsulectl checked when it
// built the page (the countersign line, and the words it recomputed against
// their sealed commitments), which the page's bootstrap sets as
// capsulectlDealPage before the page renders. render writes the model into the
// host. A step's line is shown only when its sealed record's disclosure
// verified, otherwise the step shows its capsule_id only. All text is set with
// textContent; nothing from the bundle is parsed as HTML.
//
// Its manifest is assets/deal-view.manifest.json; the copy below is the same
// manifest except executable.script_sha256, which a script cannot carry for
// itself (it is all zeros here). Its stylesheet is DEAL_VIEW_CSS, inserted
// once at render and pinned by the manifest's style_sha256 (assets/deal-view.css
// holds the same bytes).
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

// The model: everything render shows, read from the verified context and the
// page's build-time checks. Stricter than the core's gate in one place:
// membership must be proven, not unbound (canRender).
const dealViewVerified = (context) => {
  const verification = context.verification;
  return (
    verification !== undefined &&
    verification.graphClosure.status === "pass" &&
    verification.intervalCoverage.status === "pass" &&
    verification.perRecordMembership.status === "pass" &&
    Object.values(verification.capsuleResults).every((r) => r.ok) &&
    verification.disclosures.every((d) => d.status === "disclosure_match" || d.status === "withheld")
  );
};

const dealViewModel = (context) => {
  const page = globalThis.capsulectlDealPage || {};
  // The context's own frozen copy of the bundle, read only for what no
  // record seals: the extension blocks and the checkpoint's witnesses.
  const bundle = context.bundle;
  const payload = (id) => EvidenceGraph.verifiedPayload(context, id, "agent_input");
  // In the verifier's disclosure order, as the context resolved each one.
  const matched = [
    ...new Set(
      context.verification.disclosures
        .filter((d) => d.member === "agent_input" && EvidenceGraph.disclosureOf(context, d.capsuleId, "agent_input").state === "disclosed")
        .map((d) => d.capsuleId),
    ),
  ];
  // The deal's text: the input of the sealed report the x-deal-v0 extension
  // points at, checked like every record (its disclosure matched). A bundle
  // written before reports were sealed carries the text in the extension
  // itself, which no record seals, and the page says so.
  const dealExt = (bundle.extensions || {})["x-deal-v0"] || {};
  const sealedId = typeof dealExt.sealed_report === "string" ? dealExt.sealed_report : undefined;
  const otherReports = matched.filter((id) => {
    const input = payload(id);
    return id !== sealedId && input && input.type === "deal_report";
  });
  // A sealed report's record stays in every later bundle (it is signed, and
  // on the log the bundle covers), so a bundle holding one whose extension
  // names none had its text swapped for unsealed text: refused, never shown
  // under the "not checked" label.
  const reportRecords = context.records.filter((r) => r && r.action_id === "capsulectl-deal-report");
  let report = dealExt;
  if (sealedId !== undefined || otherReports.length > 0 || reportRecords.length > 0) {
    const input = sealedId !== undefined ? payload(sealedId) : undefined;
    if (otherReports.length > 0 || !input || input.type !== "deal_report" || typeof input.report !== "object" || input.report === null) {
      return { refusal: "⚠️ The deal's text could not be checked against its sealed record. Do not rely on it." };
    }
    report = input.report;
  }
  const shared = report.audience === "counterparty" || report.audience === "adjudicator";
  const base = payload(report.asked_step);
  if (!shared && (!base || !base["x-deal-v0"] || base["x-deal-v0"].record_type !== "baseline")) {
    return { refusal: "⚠️ The deal's opening step is not in this report." };
  }

  // A witness receipt rides in checkpoint.witnesses, or (the default) in the
  // cadence chain, x-cadence-witness/v0 (earlier bundles: cadence-witness/v0
  // or x-deal-cadence-v0), that anchors this deal's checkpoint in the
  // profile's cadence log. Any other witness state is shown as it is.
  const exts = bundle.extensions || {};
  const cadence = exts["x-cadence-witness/v0"] || exts["cadence-witness/v0"] || exts["x-deal-cadence-v0"] || {};
  const anchored = cadence.state === "witnessed" ? (cadence.cadence || {}).witnesses || [] : [];
  const witnesses = ((bundle.checkpoint || {}).witnesses || []).concat(anchored).filter((w) => w && typeof w.ts_url === "string");
  let witness = { kind: "none" };
  if (witnesses.length > 0) {
    let host = witnesses[0].ts_url;
    try {
      host = new URL(host).host || host;
    } catch (e) {
      // not a URL: show it as written
    }
    // An earlier checkpoint that already held every step (only sealed
    // reports after it) witnesses them all.
    const k = Number(cadence.steps_witnessed);
    const n = Number(cadence.steps);
    // What those steps hold, in the user's terms, as capsulectl named them.
    const wc = report.witness_coverage || {};
    witness = {
      kind: "witnessed",
      at: typeof cadence.checkpoint_at === "string" ? cadence.checkpoint_at : "",
      witness: host,
      cut: typeof cadence.checkpoint_at === "string" && cadence.checkpoint_at ? `cut at ${cadence.checkpoint_at} ` : "",
      part: cadence.state === "witnessed" && cadence.extent === "part" && k < n,
      k,
      n,
      coverage:
        (typeof wc.witnessed_acts === "string" && wc.witnessed_acts ? `Witnessed, steps 1 to ${k}: ${wc.witnessed_acts}. ` : "") +
        (typeof wc.pending_acts === "string" && wc.pending_acts ? `Witness pending, steps ${k + 1} to ${n}: ${wc.pending_acts}. ` : ""),
    };
  } else if (cadence.state === "scheduled") {
    witness = { kind: "scheduled", every: typeof cadence.cadence === "string" && cadence.cadence ? ` (${cadence.cadence})` : "" };
  } else if (cadence.state === "pending") {
    witness = { kind: "pending", consent: cadence.reason === "network_consent_needed" && typeof cadence.text === "string" ? cadence.text : undefined };
  }

  // Which build sealed the steps, read from the records this page verified
  // (not from the summary), compared with the build that made this page.
  // Both are already in this file: nothing is fetched to do it.
  const builds = [];
  matched.forEach((id) => {
    const rec = payload(id) || {};
    // An x-deal-v0 record names its producer in its block; a typed record, in its header.
    const p = (rec["x-deal-v0"] || rec).producer;
    const name = p && typeof p.version === "string" ? `${p.name || "capsulectl"} ${p.version} (${p.commit || "unknown"})` : "an earlier capsulectl that did not record its version";
    if (!builds.some((b) => b.name === name)) builds.push({ name, version: p && p.version });
  });

  // Which checks this page ran and which it left to `capsulectl verify`,
  // read from what the page's verification recorded and what the file
  // carries; never assumed. The page recomputes each record's digest, its
  // membership and the range proof, matches opened words against their
  // sealed digests, checks a countersignature's signature under the key it
  // names, and checks the agent's signature only on the records it cites as
  // signers (a Close and the records that acknowledge or rebut it). It has no
  // verifier for the records' own signatures in general, a witness receipt or
  // a consistency proof, and it passes a bundle whose checkpoint signature it
  // could not check, recording checkpoint_unverified.
  const v = context.verification;
  const checks = { page: [], cli: [] };
  if (Object.keys(v.capsuleResults).length > 0) checks.page.push("digests");
  if (v.perRecordMembership.status === "pass") checks.page.push("membership");
  if (v.intervalCoverage.status === "pass") checks.page.push("range");
  if (v.disclosures.some((d) => d.status === "disclosure_match")) checks.page.push("disclosures");
  if (v.countersignatures.length > 0) checks.page.push("countersignatures");
  const citesSigners = context.records.some((r) => {
    const header = r && payload(r.capsule_id);
    return (
      header && Array.isArray(header.links) && header.links.some((l) => l && (l.type === "acknowledges" || l.type === "rebuts"))
    );
  });
  if (citesSigners) checks.page.push("cited-signers");
  const checkpoint = bundle.checkpoint || {};
  if (typeof checkpoint.cose === "string" && checkpoint.cose) {
    const unchecked = [v.perRecordMembership, v.intervalCoverage].some((c) => (c.findings || []).includes("checkpoint_unverified"));
    (unchecked ? checks.cli : checks.page).push("checkpoint-signature");
  }
  if (context.records.some((r) => r && typeof r.signature === "string" && r.signature)) checks.cli.push("producer-signatures");
  if (witnesses.length > 0) checks.cli.push("witness-receipt");
  if (cadence.earlier && typeof cadence.earlier === "object") checks.cli.push("consistency");

  // Each statement's class, as sealed with it (undefined when its sealed step
  // holds no such claim).
  const claimClasses = (report.representations || []).map((r) => {
    const rec = payload(r.step);
    const body = rec && rec.body;
    const claim = body && (Number.isInteger(r.index) ? (body.claims || [])[r.index] : body);
    return claim ? claim.class : undefined;
  });

  return {
    page,
    report,
    sealed: sealedId !== undefined,
    shared,
    demo: !!(base && base.body && base.body.demo),
    matched,
    recorded: [...context.recordIndex.keys()],
    witness,
    checks,
    builds,
    claimClasses,
  };
};

// Inserted once, at the head: its bytes hash to the manifest's style_sha256,
// which the page's Content-Security-Policy lists.
const DEAL_VIEW_CSS = `#deal { --deal-warn: #9a3b00; --deal-ok: #1d6b35; line-height: 1.45; overflow-wrap: anywhere; }
@media (prefers-color-scheme: dark) { #deal { --deal-warn: #ffb07a; --deal-ok: #7fd49a; } }
#deal h1 { font-size: 1.5rem; margin: 0.2rem 0; }
#deal h2 { font-size: 1.15rem; margin-top: 1.6rem; }
#deal h3 { font-size: 0.95rem; color: var(--aac-muted); margin: 1rem 0 0.3rem; }
#deal .deal-flag summary { color: var(--deal-warn); }
#deal .deal-steps { margin: 8px 0 0; padding-left: 1.4rem; }
#deal .deal-steps li { margin: 4px 0; overflow-wrap: anywhere; }
#deal .deal-at { color: var(--aac-muted); font-size: 0.85rem; }
#deal .deal-note { color: var(--aac-muted); font-size: 0.9rem; }
#deal .deal-demo { display: inline-block; border: 1px solid var(--deal-warn); color: var(--deal-warn); padding: 0 6px; border-radius: 4px; font-size: 0.8rem; }
#deal .deal-bad { color: var(--deal-warn); font-weight: 600; }
#deal details { border: 1px solid var(--aac-line); border-radius: 6px; padding: 8px 12px; margin: 8px 0; }
#deal summary { cursor: pointer; font-weight: 600; }
#deal table.deal-merchant { border-collapse: collapse; width: 100%; margin: 6px 0; }
#deal table.deal-merchant th, #deal table.deal-merchant td { border: 1px solid var(--aac-line); padding: 4px 8px; text-align: left; vertical-align: top; overflow-wrap: anywhere; }
#deal table.deal-merchant th { color: var(--aac-muted); font-weight: 600; width: 40%; }
#deal .deal-ok { color: var(--deal-ok); font-weight: 600; }
#deal code { color: var(--aac-muted); font-size: 0.8rem; overflow-wrap: anywhere; }
#deal .deal-assurance { border: 1px solid var(--aac-line); border-radius: 6px; padding: 8px 12px; margin: 8px 0 16px; }
#deal .deal-rung { font-weight: 600; margin: 0.2rem 0; }
#deal .deal-scope { font-weight: 600; margin: 0.2rem 0 0.6rem; }
#deal .deal-claims { border: none; padding: 0; }
#deal .deal-verify { background: transparent; border: 1px solid var(--aac-line); border-radius: 4px; padding: 6px 8px; overflow-x: auto; font-size: 0.85rem; user-select: all; }
`;

const dealViewRender = (model, regions) => {
  if (!document.getElementById("capsulectl-deal-view-style")) {
    const style = document.createElement("style");
    style.id = "capsulectl-deal-view-style";
    style.textContent = DEAL_VIEW_CSS;
    document.head.append(style);
  }
  const host = document.createElement("div");
  host.id = "deal";
  regions.L0.append(host);
  const el = (tag, text, cls) => {
    const node = document.createElement(tag);
    if (text !== undefined) node.textContent = text;
    if (cls) node.className = cls;
    return node;
  };
  if (model.refusal !== undefined) {
    host.append(el("p", model.refusal, "deal-bad"));
    return;
  }
  const { page, report, sealed, shared } = model;
  const matched = new Set(model.matched);
  const recorded = new Set(model.recorded);
  const byId = new Map((report.steps || []).map((s) => [s.capsule_id, s]));

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
  // The witness state, as buildModel read it: this page does not check a
  // receipt (capsulectl verify does, against a witness directory the reader
  // chooses), and says so.
  const w = model.witness;
  if (w.kind === "witnessed") {
    header.append(
      el(
        "p",
        w.part
          ? `Witnessed in part: ${w.witness}, an independent log, signed a receipt for this deal's checkpoint, ${w.cut}at a cadence tick, covering steps 1 to ${w.k} of ${w.n}: those existed, unchanged, by then. Steps ${w.k + 1} to ${w.n} are sealed by my agent on this device only, witness pending. ${w.coverage}It does not confirm what the agent did.`
          : `Witnessed: ${w.witness}, an independent log, signed a receipt for this deal's checkpoint, ${w.cut}at a cadence tick after the deal's steps: the record existed, unchanged, by then. It does not confirm what the agent did.`,
        "deal-rung",
      ),
      el(
        "p",
        "The receipt is in this file; this page does not check it. Check it with " +
          "capsulectl verify --bundle FILE --witness-directory DIRECTORY.json, using a witness directory you trust.",
        "deal-note",
      ),
    );
  } else if (w.kind === "scheduled") {
    header.append(el("p", `Sealed by my agent, witness pending. A deal is not witnessed at the moment it happens: its checkpoint goes to the witness at the next tick of this profile's cadence${w.every}, and only then can it be witnessed. Until then it is sealed on this device only.`, "deal-rung"));
  } else if (w.kind === "pending") {
    header.append(el("p", "Sealed by my agent, witness pending. This checkpoint was sent at a cadence tick, but no receipt has come back yet; it is retried at every tick.", "deal-rung"));
    if (typeof w.consent === "string") header.append(el("p", `Witness ${w.consent}`, "deal-note"));
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
  if (page && page.countersign && typeof page.countersign.text === "string") countersign = page.countersign;
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
  // Which checks ran here and which only `capsulectl verify` runs, as
  // buildModel read them: a check this page did not run is never called
  // checked here.
  const words = {
    digests: "Each record's contents match the fingerprint it was sealed with.",
    membership: "Each record sits at its place in this deal's log.",
    range: "The log's range proof holds: no record is missing from the stretch this file covers.",
    disclosures: "Each opened step's words match what was sealed.",
    countersignatures: "Each countersignature is a valid signature by the key it names (the details below show whose key, if known).",
    "cited-signers":
      "The agent's signature on the records the details below cite as signers, each marked there as verified or not. Not the signature on every record.",
    "checkpoint-signature": {
      page: "The signature on the log's checkpoint.",
      cli: "The signature on the log's checkpoint. This page cannot check it, and it shows the bundle as passing without it.",
    },
    "producer-signatures": "The agent's signature on every record.",
    "witness-receipt": "The witness's receipt.",
    consistency: "That the log only grew, and nothing in it changed, since its earlier checkpoint.",
  };
  const checkList = (ids, which) => {
    const list = el("ul", undefined, "deal-checks");
    list.dataset.checks = which;
    ids.forEach((id) => {
      const li = el("li", typeof words[id] === "string" ? words[id] : words[id][which]);
      li.dataset.check = id;
      list.append(li);
    });
    return list;
  };
  header.append(el("h3", "Checked on this page"), checkList(model.checks.page, "page"));
  if (model.checks.cli.length > 0) {
    header.append(el("h3", "Checked only by capsulectl verify"), checkList(model.checks.cli, "cli"));
  }
  header.append(el("p", "Run, with only this file and capsulectl:", "deal-note"));
  const command = (report.verify_command || "capsulectl verify --bundle receipt.html") + (model.checks.cli.includes("witness-receipt") ? " --witness-directory DIRECTORY.json" : "");
  header.append(el("pre", command, "deal-verify"));
  if (model.checks.cli.includes("witness-receipt")) {
    header.append(el("p", "DIRECTORY.json is a witness directory you trust: the command checks the receipt against it.", "deal-note"));
  }
  header.append(
    el(
      "p",
      "None of these checks says who the agent or its user is, or that this file is the latest copy of the deal. " +
        "If you would rather not take this page's word for the first list, open verify.agentactioncapsule.org and drop this file in.",
      "deal-note",
    ),
  );
  if (shared) {
    const who = report.audience === "counterparty" ? "the other party" : "an adjudicator";
    header.append(el("p", `A shared copy for ${who}. Left out of this copy: ${(report.withheld || []).join(", ")}.`, "deal-note"));
  }

  const recordOk = (id) => recorded.has(id);
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

  if (model.demo) host.append(el("span", "DEMO", "deal-demo"));
  host.append(el("h1", "Deal report"));
  // What the witness covers, first, in one line: from the witness block
  // only, never more than it covers. The receipt and what it does not prove
  // are in the header below.
  const w0 = model.witness;
  const at = w0.at ? ` at ${w0.at}` : "";
  const witnessed =
    w0.kind !== "witnessed"
      ? w0.kind === "scheduled" || w0.kind === "pending"
        ? "Not witnessed yet: witness receipt pending the next tick."
        : "Not witnessed."
      : w0.part
        ? `Witnessed through step ${w0.k} of ${w0.n}${at} (receipt attached); later steps: witness receipt pending the next tick.`
        : `Witnessed through the last step this copy holds${at} (receipt attached).`;
  const line = el("p", witnessed, "deal-rung");
  line.dataset.witness = w0.kind === "witnessed" ? (w0.part ? "part" : "all") : w0.kind;
  host.append(line);

  // Which build sealed the steps, read from the records this page verified
  // (not from the summary), compared with the build that made this page.
  // Both are already in this file: nothing is fetched to do it.
  const provenance = el("section", undefined, "deal-provenance");
  const builds = model.builds;
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

  // The user's words were checked against the baseline's sealed commitment
  // when this page was built (capsulectl refuses to write a page whose words
  // do not match); this page does not recompute it.
  const opening = report.asked_opening || {};
  const askedChecked = !!(page && page.openings && page.openings.asked === true) && typeof opening.text === "string";
  host.append(el("h2", "What you asked"));
  if (shared) {
    host.append(el("p", "Withheld from this copy.", "deal-note"));
  } else if (askedChecked) {
    host.append(item(`“${opening.text}”`, [report.asked_step]));
    host.append(el("p", "✓ These are the exact words sealed when the deal opened, checked when this page was built.", "deal-note"));
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
    const checked = (page && page.openings && page.openings.representations) || [];
    for (const [i, r] of said.entries()) {
      // Checked against its sealed commitment when this page was built.
      const claimClass = model.claimClasses[i];
      const ok = checked[i] === true && claimClass !== undefined && typeof r.text === "string";
      if (ok) {
        const label = labels[claimClass];
        host.append(item(`${label ? `${label}: ` : ""}“${r.text}” ✓`, [r.step]));
      } else {
        host.append(item("⚠️ A statement here could not be checked against its sealed step.", [r.step], "deal-flag"));
      }
    }
    host.append(el("p", "✓ These are the exact words sealed when the agent said them, checked when this page was built; this page does not check that they are true.", "deal-note"));
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
      "This page checked each step's sealed record and its place in this deal's log; the lists above say what it did not check. " +
        "The records carry fingerprints, not names or numbers; the words shown come from this device. " +
        (shared ? "Steps marked contents withheld are proven present and unchanged; what they say is not in this copy. " : "") +
        "The seal key is on the agent's machine, so this shows the record was not changed after it was made, not who made it. " +
        "It covers this skill's own records only, never the agent host's own store.",
      "deal-note",
    ),
  );
};

EvidenceGraph.registerPresentation({
  manifest: {"spec_version": "aac.presentation-manifest/v0", "id": "capsulectl.deal-view/v0", "presentation_api": "aac.presentation-api/v0", "runtime_min": "0.1.0", "trust_class": "trusted-executable", "requires": {"bundle_kind": "evidence-bundle/v2", "extensions": {"required": ["x-deal-v0"]}}, "forbids": {"profiles": ["spec_version:report/v1", "result_version:evidence-result-v0", "spec_version:evaluation-summary/v1"]}, "audiences": ["*"], "formats": ["html"], "fallback": false, "priority": 1, "executable": {"carrier": "module-slot", "script_sha256": "0000000000000000000000000000000000000000000000000000000000000000", "style_sha256": ["033433c0f3b97d682e7b81a8309dd868a96b0ea91f18997b33e097686e7e74da"]}},
  canRender: dealViewVerified,
  buildModel: dealViewModel,
  render: dealViewRender,
});
