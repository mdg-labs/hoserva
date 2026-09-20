package main

import (
	"context"
	"encoding/json"
	"log"

	"github.com/mdg-labs/hoserva/internal/acme"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
)

type acmeNotify struct {
	svc *notify.Service
}

func (n *acmeNotify) Publish(ctx context.Context, event, title, message string) error {
	if n == nil || n.svc == nil {
		return nil
	}
	return n.svc.Publish(ctx, notify.EventType(event), title, message)
}

func acmeDatabaseSecrets(ctx context.Context, st *acme.Store) ([]backup.DatabaseSecret, error) {
	if st == nil {
		return nil, nil
	}
	rows, err := st.ListSecrets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]backup.DatabaseSecret, 0, len(rows))
	for _, row := range rows {
		out = append(out, backup.DatabaseSecret{
			Table:      "acme_config",
			Column:     row.Column,
			RowID:      "1",
			Ciphertext: row.Ciphertext,
		})
	}
	return out, nil
}

func (r *scheduleRunner) tickACME(ctx context.Context) error {
	if r == nil || r.ACME == nil || r.Scheduler == nil {
		return nil
	}
	due, err := r.ACME.RenewalDue(ctx)
	if err != nil {
		return err
	}
	if !due {
		return nil
	}
	if r.Jobs != nil {
		active, err := r.Jobs.ListActive(ctx)
		if err != nil {
			return err
		}
		for _, j := range active {
			if j.Type == job.TypeACMEIssue {
				return nil
			}
		}
	}
	body, err := json.Marshal(job.ACMEIssueParams{Renew: true})
	if err != nil {
		return err
	}
	_, err = r.Scheduler.Submit(ctx, job.TypeACMEIssue, []string{"tls"}, body)
	if err != nil {
		log.Printf("hoservad: enqueueing Let's Encrypt renewal: %v", err)
	}
	return nil
}
