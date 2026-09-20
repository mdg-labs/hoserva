package job

import "context"

// RunACMEIssue is the RunFunc hoservad registers for TypeACMEIssue.
// issue talks to ACME and installs only after success; tests inject a
// closure over acme.Service.Issue (or a fake).
func RunACMEIssue(issue func(ctx context.Context, renew bool) error) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		renew, err := ACMERenewFromParams(rc.Params())
		if err != nil {
			return err
		}
		return issue(ctx, renew)
	}
}
