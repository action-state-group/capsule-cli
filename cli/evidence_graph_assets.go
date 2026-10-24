package cli

import (
	_ "embed"
)

// The browser runtime is capsule-viewer's page code (the views, the
// presentation mount, the outcome and EU report cards) bundled with
// agent-action-capsule's verifier, vendored as built and rebuilt
// reproducibly by scripts/build-evidence-graph-iife.sh. deal-view.js adds the deal section: a
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
// IIFE (dist/ is gitignored upstream and no Go release carries it), so the
// IIFE is built from capsule-viewer's ts/src, with agent-action-capsule's
// ts/src as its verifier (scripts/build-report-runtime.mjs), and vendored
// here with its digest pinned in code; TestIIFEIsPinned keeps the two in
// step. The shell's style hashes (runtime-style-hashes.txt in the Go module)
// cover the runtime's styles, so the Go module and the IIFE's
// capsule-viewer commit move together. The file and its digest are the ones
// `presentation build` and `bundle` or `disclose --html` embed
// (EvidenceGraphIIFE, EvidenceGraphIIFESHA256, below);
// scripts/build-evidence-graph-iife.sh rebuilds it from the two commits
// evidenceGraphIIFESource records, and scripts/iife-sync.sh from any
// capsule-viewer and aac checkouts. The report is only as
// current as that file.

const (
	// evidenceGraphIIFEDigest is the SHA-256 of assets/evidence-graph.iife.js.
	EvidenceGraphIIFEDigest = "14e4eca08a978421af8c61c6b0cb6fca5bdae2d840f0c54c6ee40ee0041ed497"
	// evidenceGraphIIFESource names the commits and build the vendored IIFE
	// came from: capsule-viewer's page code, and agent-action-capsule's
	// verifier using released CLL 0.2.1 from its registry lock. Both must be
	// merged on their repository's main (CI checks). They are pinned apart
	// from go.mod: the aac Go module may move to a release tag while the
	// browser verifier stays at this build.
	evidenceGraphIIFESource = "capsule-viewer main@d100e320e4748b497a10b8219cf80bd5f4dc0a0b ts/src with verifier agent-action-capsule main@cd68af7c80b6218efb86b6c749d253472054e590 ts/src: scripts/build-report-runtime.mjs (esbuild 0.28.2)"

	// cardExtension records which card the report was built for. There is
	// no card registry yet: every card renders the one Result page today,
	// and the viewer keys on this block once cards exist.
	CardExtension = "report-card/v1"
	// presentationExtension is the bundle-level header block the viewer
	// reads (producer display name, logo, title), and nothing else.
	PresentationExtension = "presentation/v1"
)
