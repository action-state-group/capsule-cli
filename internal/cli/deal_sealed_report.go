package cli

// dealReportActionID is the action_id of the capsule a deal report seals its
// copy's readable text in: an fyi capsule on the deal's own log whose input
// is the deal_report record (dealSealReport). The bundle's x-deal-v0
// extension then only points at it (dealReportPointer), so the text a page
// shows is a disclosed record payload, checked like every other: an edited
// line makes the bundle INVALID.
const DealReportActionID = "capsulectl-deal-report"

// dealReportPointer is the x-deal-v0 extension member naming the copy's
// sealed report capsule.
const DealReportPointer = "sealed_report"
