package monitor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

// shutdownFeishuRuntime closes callback admission before cancellation and then
// waits for already-admitted workers and callbacks to drain.
func shutdownFeishuRuntime(
	cancelRuntime context.CancelFunc,
	continuationDone <-chan struct{},
	cardDeliveryDone <-chan struct{},
	messageBot *bot,
	approvals *operationApprovalService,
	resourceAccess *resourceAccessManager,
) {
	if messageBot != nil {
		messageBot.tasks.CloseAdmission()
	}
	if approvals != nil {
		approvals.tasks.CloseAdmission()
	}
	if resourceAccess != nil {
		resourceAccess.tasks.CloseAdmission()
	}
	if cancelRuntime != nil {
		cancelRuntime()
	}
	if continuationDone != nil {
		<-continuationDone
	}
	if cardDeliveryDone != nil {
		<-cardDeliveryDone
	}
	if messageBot != nil {
		messageBot.tasks.Wait()
	}
	if approvals != nil {
		approvals.tasks.Wait()
	}
	if resourceAccess != nil {
		resourceAccess.tasks.Wait()
	}
}

func newFeishuRuntimeExecutionOwnerID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "runtime_" + hex.EncodeToString(value[:]), nil
}
