import io
p = 'internal/integrations/lark/approval_reminder.go'
s = io.open(p, encoding='utf-8').read()

def rep(old, new):
    global s
    assert old in s, 'NOT FOUND: ' + old[:70]
    s = s.replace(old, new)

# --- deliverToRecipients: compress into tighter form ---
start = s.index('func (r *ApprovalReminder) deliverToRecipients')
end = s.index('// approvalBindingCandidate is one candidate row from the binding query.')
new_fn = '''func (r *ApprovalReminder) deliverToRecipients(ctx context.Context, p protocol.ApprovalGateEnteredPayload, anchorWorkspaceID string) {
	// AIFIRST: CR-2026-051 FR-3 — project chain: cr → issue → project →
	// workspace.slug, one round-trip, every hop carries the anchor (INNER
	// JOIN = fail-closed). Deliberately NOT the project_gates.go join shape
	// (primary-key join cannot detect a cross-workspace shell_issue_id).
	var projectID, slug, title string
	err := r.pool.QueryRow(ctx, `
		SELECT p.id::text, w.slug, c.title
		  FROM cr c
		  JOIN issue   i ON i.id = c.shell_issue_id AND i.workspace_id = $1
		  JOIN project p ON p.id = i.project_id     AND p.workspace_id = $1
		  JOIN workspace w ON w.id = $1
		 WHERE c.workspace_id = $1 AND c.cr_id = $2`,
		anchorWorkspaceID, p.CRID).Scan(&projectID, &slug, &title)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			r.logFailEvent(p, anchorWorkspaceID, stepProjectChain, errorClassOf(err))
			return
		}
		// Zero rows: pick the reason with a second anchored query
		// (reason-selection only — never produces recipients).
		var shellNull bool
		if serr := r.pool.QueryRow(ctx, `
			SELECT shell_issue_id IS NULL FROM cr WHERE workspace_id = $1 AND cr_id = $2`,
			anchorWorkspaceID, p.CRID).Scan(&shellNull); serr != nil && !errors.Is(serr, pgx.ErrNoRows) {
			r.logFailEvent(p, anchorWorkspaceID, stepProjectChain, errorClassOf(serr))
			return
		}
		if shellNull {
			r.logSkipEvent(p, anchorWorkspaceID, reasonProjectUnresolved)
		} else {
			r.logSkipEvent(p, anchorWorkspaceID, reasonWorkspaceMismatch)
		}
		return
	}
	if slug == "" {
		r.logSkipEvent(p, anchorWorkspaceID, reasonWorkspaceMismatch)
		return
	}

	// AC-4 case ①'s reason carrier: the approver set comes exclusively from
	// this role-filtered query (FR-4) — non-owner/admin members never enter
	// the loop, so no recipient-level record exists for them.
	rows, err := r.pool.Query(ctx, `
		SELECT user_id::text FROM member WHERE workspace_id = $1 AND role IN ('owner','admin')`,
		anchorWorkspaceID)
	if err != nil {
		r.logFailEvent(p, anchorWorkspaceID, stepApproverQuery, errorClassOf(err))
		return
	}
	var approvers []string
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			rows.Close()
			r.logFailEvent(p, anchorWorkspaceID, stepApproverQuery, errorClassOf(err))
			return
		}
		approvers = append(approvers, userID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		r.logFailEvent(p, anchorWorkspaceID, stepApproverQuery, errorClassOf(err))
		return
	}
	if len(approvers) == 0 {
		r.logSkipEvent(p, anchorWorkspaceID, reasonNoApprover)
		return
	}

	approveURL := r.appURL + "/" + slug + "/projects/" + projectID + "?tab=chat"

	// attemptedOpenIDs means "already attempted", not "already sent" (BL-2):
	// registration happens before any fallible action, so a first failure
	// never lets a later recipient retry the same open_id.
	attemptedOpenIDs := map[string]struct{}{}
	for _, userID := range approvers {
		// AIFIRST: CR-2026-051 FR-5 — effective-binding candidates for one
		// member. LEFT JOIN (not INNER) so a dangling installation_id
		// (channel_* has NO foreign keys) stays observable as
		// installation-missing; ci.* is fetched but not filtered here —
		// chooseEffective filters so the failure modes stay distinguishable.
		brows, err := r.pool.Query(ctx, `
			SELECT b.id::text, b.channel_user_id, b.installation_id::text,
			       ci.id::text, ci.workspace_id::text, ci.channel_type, ci.status
			  FROM channel_user_binding b
			  LEFT JOIN channel_installation ci ON ci.id = b.installation_id
			 WHERE b.workspace_id = $1 AND b.multica_user_id = $2 AND b.channel_type = 'feishu'
			 ORDER BY b.bound_at DESC, b.id ASC`,
			anchorWorkspaceID, userID)
		if err != nil {
			r.logFail(p, anchorWorkspaceID, userID, "", stepBindingQuery, errorClassOf(err))
			continue
		}
		var candidates []approvalBindingCandidate
		bindOK := true
		for brows.Next() {
			var c approvalBindingCandidate
			if err := brows.Scan(&c.BindingID, &c.OpenID, &c.InstallationID, &c.InstID,
				&c.InstWorkspaceID, &c.InstChannelType, &c.InstStatus); err != nil {
				bindOK = false
				r.logFail(p, anchorWorkspaceID, userID, "", stepBindingQuery, errorClassOf(err))
				break
			}
			candidates = append(candidates, c)
		}
		brows.Close()
		if !bindOK {
			continue
		}
		if err := brows.Err(); err != nil {
			r.logFail(p, anchorWorkspaceID, userID, "", stepBindingQuery, errorClassOf(err))
			continue
		}
		if len(candidates) == 0 {
			r.logSkipRecipient(p, anchorWorkspaceID, userID, reasonBindingMissing)
			continue
		}
		pick, reason := chooseEffective(candidates, anchorWorkspaceID)
		if pick == nil {
			r.logSkipRecipient(p, anchorWorkspaceID, userID, reason)
			continue
		}

		// BL-2: register the attempt before any fallible action.
		if _, dup := attemptedOpenIDs[string(pick.OpenID)]; dup {
			continue // same open_id: one attempt per event (FR-5/AC-3)
		}
		attemptedOpenIDs[string(pick.OpenID)] = struct{}{}

		// BL-1: workspace-scoped full installation read; config JSONB is
		// decoded upstream (installationFromRow), never by raw SQL here.
		installationUUID, err := util.ParseUUID(*pick.InstallationID)
		anchorUUID, uerr := util.ParseUUID(anchorWorkspaceID)
		if err != nil || uerr != nil {
			r.logFail(p, anchorWorkspaceID, userID, pick.OpenID, stepCredentialHydrate, errorClassOf(errors.Join(err, uerr)))
			continue
		}
		inst, err := r.credentials.GetInWorkspace(ctx, installationUUID, anchorUUID)
		if err != nil {
			if errors.Is(err, ErrInstallationNotFound) {
				r.logSkipRecipient(p, anchorWorkspaceID, userID, reasonInstallationMissing)
			} else {
				r.logFail(p, anchorWorkspaceID, userID, pick.OpenID, stepCredentialHydrate, errorClassOf(err))
			}
			continue
		}
		// Re-check tenant/status after hydration (TOCTOU window between
		// classification and send); hydration wins on disagreement.
		if util.UUIDToString(inst.WorkspaceID) != anchorWorkspaceID {
			r.logSkipRecipient(p, anchorWorkspaceID, userID, reasonWorkspaceMismatch)
			continue
		}
		if inst.Status != "active" {
			r.logSkipRecipient(p, anchorWorkspaceID, userID, reasonInstallationRevoked)
			continue
		}
		creds, err := installationCredentialsFor(inst, r.credentials)
		if err != nil {
			r.logFail(p, anchorWorkspaceID, userID, pick.OpenID, stepCredentialDecrypt, errorClassOf(err))
			continue
		}

		rctx, rcancel := context.WithTimeout(ctx, r.recipientTimeout)
		err = r.client.SendApprovalReminderCard(rctx, ApprovalReminderParams{
			InstallationID: creds, OpenID: pick.OpenID,
			CRID: p.CRID, CRTitle: title, StageLabel: stageLabel(p.Status), ApproveURL: approveURL,
		})
		rcancel()
		if err != nil {
			r.logFail(p, anchorWorkspaceID, userID, pick.OpenID, stepSend, errorClassOf(err))
			continue // one recipient's failure never blocks the others
		}
		r.logSent(p, anchorWorkspaceID, userID, pick.OpenID)
	}
}

'''
s = s[:start] + new_fn + s[end:]

# --- chooseEffective comment trim ---
rep('''// chooseEffective returns the first fully-valid candidate in SQL order, else
// nil + the most-specific reason: workspace-mismatch > installation-revoked >
// installation-missing > binding-missing (fallback for a non-empty set).
// Pure function.
func chooseEffective(rows []approvalBindingCandidate, anchorWorkspaceID string) (*approvalBindingCandidate, string) {
	seenMissing, seenMismatch, seenRevoked := false, false, false
	for i := range rows {
		row := &rows[i]
		if row.InstallationID == nil || row.InstID == nil {
			seenMissing = true
			continue
		}
		if row.InstWorkspaceID == nil || *row.InstWorkspaceID != anchorWorkspaceID ||
			row.InstChannelType == nil || *row.InstChannelType != "feishu" {
			seenMismatch = true
			continue
		}
		if row.InstStatus == nil || *row.InstStatus != "active" {
			seenRevoked = true
			continue
		}
		return row, ""
	}
	if seenMismatch {
		return nil, reasonWorkspaceMismatch
	}
	if seenRevoked {
		return nil, reasonInstallationRevoked
	}
	if seenMissing {
		return nil, reasonInstallationMissing
	}
	return nil, reasonBindingMissing
}''',
'''// chooseEffective returns the first fully-valid candidate in SQL order, else
// nil + the most-specific reason: workspace-mismatch > installation-revoked >
// installation-missing > binding-missing (fallback for a non-empty set).
// Pure function.
func chooseEffective(rows []approvalBindingCandidate, anchorWorkspaceID string) (*approvalBindingCandidate, string) {
	seenMissing, seenMismatch, seenRevoked := false, false, false
	for i := range rows {
		row := &rows[i]
		if row.InstallationID == nil || row.InstID == nil {
			seenMissing = true
			continue
		}
		if row.InstWorkspaceID == nil || *row.InstWorkspaceID != anchorWorkspaceID ||
			row.InstChannelType == nil || *row.InstChannelType != "feishu" {
			seenMismatch = true
			continue
		}
		if row.InstStatus == nil || *row.InstStatus != "active" {
			seenRevoked = true
			continue
		}
		return row, ""
	}
	switch {
	case seenMismatch:
		return nil, reasonWorkspaceMismatch
	case seenRevoked:
		return nil, reasonInstallationRevoked
	case seenMissing:
		return nil, reasonInstallationMissing
	default:
		return nil, reasonBindingMissing
	}
}''')

io.open(p, 'w', encoding='utf-8', newline='\n').write(s)
print('done')
