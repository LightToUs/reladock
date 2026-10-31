package main

import (
	"context"
	"errors"
	"log"
	"reladock/internal/notify"
	"reladock/internal/observability"
	"reladock/internal/platform"
	"time"
)

func runNotifications(ctx context.Context, store *platform.Store, sender *notify.Sender, metrics *observability.Registry) {
	for ctx.Err() == nil {
		work, cancel := context.WithTimeout(ctx, 20*time.Second)
		job, err := store.ClaimNotification(work)
		if err == nil && job != nil {
			sendErr := sender.Send(work, job.Event)
			code := ""
			if sendErr != nil {
				code = "delivery-failed"
			}
			err = store.ReportNotification(work, job.Event.EventID, job.LeaseToken, sendErr == nil, code)
			_ = metrics.ObserveWorker(observability.WorkerNotificationDelivery, errors.Join(sendErr, err))
			if sendErr != nil {
				log.Print("incident notification delivery not acknowledged; durable retry retained")
			}
		}
		if err != nil && job == nil {
			_ = metrics.ObserveWorker(observability.WorkerNotificationDelivery, err)
		}
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Print("incident notification worker needs attention")
		}
		if job == nil || err != nil {
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}
