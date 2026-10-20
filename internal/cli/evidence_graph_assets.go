package cli

import (
	_ "embed"
)

// The browser verifier is agent-action-capsule's own evidence-graph runtime,
// vendored unmodified and rebuilt reproducibly by
// scripts/build-evidence-graph-iife.sh. deal-view.js adds the deal section: a
// presentation module (capsulectl.deal-view/v0, deal-view.manifest.json) the
// page builder inlines in the emitter's module slot, pinned by its digest like
// the runtime, with its stylesheet pinned by the manifest's style_sha256.
var (
	//go:embed assets/evidence-graph.iife.js
	EvidenceGraphIIFE []byte
	//go:embed assets/evidence-graph.iife.js.sha256
	EvidenceGraphIIFESHA256 string
	//go:embed assets/deal-view.js
	DealViewJS string
	//go:embed assets/deal-view.js.sha256
	DealViewJSSHA256 string
	//go:embed assets/deal-view.manifest.json
	DealViewManifestJSON []byte
)

// `capsulectl book report build` (the capsulectl-book plugin) renders a held,
// disclosed Evidence Bundle whose root is a sealed Evidence Result v0 into an
// offline report.html (2026-09-26:
// one bundle plus the presentation profile; Go twin, no Node at render
// time; the card selects the renderer, presentation feeds the header only).
// It verifies before it renders and refuses a bundle that does not verify
// or whose root is not a Result; it touches no store and seals nothing --
// the disclose act that produced the bundle is already on record.
//
// The page is capsule-viewer's go/emitter shell around the bundle and the browser
// runtime (the IIFE). The pinned capsule-viewer Go module ships the shell but not the
// IIFE (dist/ is gitignored upstream and no release carries it), so the
// built IIFE is vendored here with its digest pinned in code; TestIIFEIsPinned
// keeps the two in step. The file and its digest are the same ones `deal
// report` and `presentation build` embed (EvidenceGraphIIFE,
// EvidenceGraphIIFESHA256, below);
// scripts/build-evidence-graph-iife.sh rebuilds it from the aac commit go.mod
// pins, and scripts/iife-sync.sh from any aac checkout. The report is only as
// current as that file.

const (
	// evidenceGraphIIFEDigest is the SHA-256 of assets/evidence-graph.iife.js.
	EvidenceGraphIIFEDigest = "97ad108149396584c8e78446ba6486e045229861039bb14ed0f02e6818f67da8"
	// evidenceGraphIIFESource names the aac commit and build the vendored
	// IIFE came from: the merged aac commit go.mod pins, using released
	// CLL 0.2.1 from the registry lock.
	evidenceGraphIIFESource = "agent-action-capsule main@60aabc6002325ac9d18848acfe3be55e07fd59db ts: npm run emitter:iife (esbuild 0.28.2)"

	// cardExtension records which card the report was built for. There is
	// no card registry yet: every card renders the one Result page today,
	// and the viewer keys on this block once cards exist.
	CardExtension = "report-card/v1"
	// presentationExtension is the bundle-level header block the viewer
	// reads (producer display name, logo, title), and nothing else.
	PresentationExtension = "presentation/v1"
)
