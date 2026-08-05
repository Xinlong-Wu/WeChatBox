package monitor

import (
	"time"

	"lingobridge/internal/store"
)

type resourceMetadataStore interface {
	PlatformID() string
	SaveFeishuBotResource(store.FeishuBotResource) (store.FeishuBotResource, error)
	GetFeishuBotResource(accountID, resourceType, resourceToken string) (store.FeishuBotResource, error)
	DefaultFeishuChatFolder(accountID, chatID string) (store.FeishuChatFolder, error)
	GetFeishuChatFolder(accountID, chatID, folderToken string) (store.FeishuChatFolder, error)
	GetFeishuChatDocument(accountID, chatID, documentToken string) (store.FeishuChatDocument, error)
}

type resourceRequestStore interface {
	CreateFeishuResourceAccessRequest(store.FeishuResourceAccessRequest) (store.FeishuResourceAccessRequest, error)
	PrepareFeishuResourceAccessOAuth(id, accountID, stateHash, stateCiphertext, verifier, subjectType, subjectID string, now time.Time) error
	MarkFeishuResourceAccessOAuthHandoffDelivered(id, accountID, stateHash string, now time.Time) error
	ClaimFeishuResourceAccessExecution(id, accountID string, now time.Time) (store.FeishuResourceAccessRequest, error)
	SetFeishuResourceAccessCardMessageID(id, accountID, messageID string, now time.Time) error
	ApproveFeishuResourceAccessRequest(id, accountID, grantMode string, match store.FeishuResourceAccessMatch, now time.Time) (store.FeishuResourceAccessRequest, error)
	ListApprovedPendingFeishuResourceAccessRequests(accountID string, now time.Time, limit int) ([]store.FeishuResourceAccessRequest, error)
	GetFeishuResourceAccessRequest(id, accountID string) (store.FeishuResourceAccessRequest, error)
	ClaimFeishuResourceAccessOAuth(stateHash, accountID string, now time.Time) (store.FeishuResourceAccessRequest, error)
	ClaimFeishuResourceAccessOAuthFromCard(id, accountID, stateHash string, match store.FeishuResourceAccessMatch, now time.Time) (store.FeishuResourceAccessRequest, error)
	DenyFeishuResourceAccessRequest(id, accountID string, match store.FeishuResourceAccessMatch, now time.Time) (store.FeishuResourceAccessRequest, error)
	CompleteFeishuResourceAccessRequest(id, accountID, source, verifiedPermission string, capability *store.FeishuResourceCapability, grant *store.FeishuResourceGrant, now time.Time) error
	FailFeishuResourceAccessRequest(id, accountID string, now time.Time) error
	ExpireFeishuResourceAccessRequests(accountID string, now time.Time) (int64, error)
	ListExecutingFeishuResourceAccessRequests(accountID string) ([]store.FeishuResourceAccessRequest, error)
}

type resourcePermissionStore interface {
	GetFeishuBotResource(accountID, resourceType, resourceToken string) (store.FeishuBotResource, error)
	ExpireFeishuResourceGrants(accountID string, now time.Time) (int64, error)
	UpsertFeishuResourceGrant(store.FeishuResourceGrant) (store.FeishuResourceGrant, error)
	ActiveFeishuResourceGrant(accountID, actorType, actorID, chatID, resourceType, resourceToken, permission string, now time.Time) (store.FeishuResourceGrant, bool, error)
	RevokeFeishuResourceGrant(accountID, chatID, resourceType, resourceToken string, now time.Time) error
	UpsertFeishuResourceCapability(store.FeishuResourceCapability) (store.FeishuResourceCapability, error)
	ActiveFeishuResourceCapability(accountID, resourceType, resourceToken, subjectType, subjectID, permission string) (store.FeishuResourceCapability, bool, error)
	RevokeFeishuResourceCapability(accountID, resourceType, resourceToken, subjectType, subjectID string, now time.Time) error
}

type oauthCredentialStore interface {
	SaveFeishuUserOAuthCredential(store.FeishuUserOAuthCredential) (store.FeishuUserOAuthCredential, error)
	GetFeishuUserOAuthCredential(accountID, actorOpenID, actorUserID string) (store.FeishuUserOAuthCredential, error)
	GetFeishuUserOAuthCredentialByID(id, accountID string) (store.FeishuUserOAuthCredential, error)
	MarkFeishuUserOAuthCredentialReauthRequired(id, accountID string, expectedVersion int64, now time.Time) (store.FeishuUserOAuthCredential, error)
	PrepareFeishuOAuthRefreshAttempt(credentialID, accountID string, expectedVersion int64, leaseToken string, now time.Time, leaseDuration time.Duration) (store.FeishuOAuthRefreshAttempt, bool, error)
	StageFeishuOAuthRefreshResponse(attemptID, accountID, leaseToken string, stage store.FeishuOAuthRefreshStage, now time.Time) (store.FeishuOAuthRefreshAttempt, error)
	ApplyFeishuOAuthRefreshAttempt(attemptID, accountID string, update store.FeishuOAuthRefreshCredentialUpdate, now time.Time) (store.FeishuUserOAuthCredential, store.FeishuOAuthRefreshAttempt, error)
	FailFeishuOAuthRefreshAttempt(attemptID, accountID, leaseToken, errorCategory string, requireReauthorization bool, now time.Time) (store.FeishuUserOAuthCredential, store.FeishuOAuthRefreshAttempt, error)
	MarkFeishuOAuthRefreshAttemptAmbiguous(attemptID, accountID string, now time.Time) (store.FeishuUserOAuthCredential, store.FeishuOAuthRefreshAttempt, error)
	MarkOwnedFeishuOAuthRefreshAttemptAmbiguous(attemptID, accountID, leaseToken string, now time.Time) (store.FeishuUserOAuthCredential, store.FeishuOAuthRefreshAttempt, error)
	InvalidateFeishuOAuthRefreshAttempt(attemptID, accountID, errorCategory string, now time.Time) (store.FeishuUserOAuthCredential, store.FeishuOAuthRefreshAttempt, error)
	GetFeishuOAuthRefreshAttempt(attemptID, accountID string) (store.FeishuOAuthRefreshAttempt, error)
	ListRecoverableFeishuOAuthRefreshAttempts(accountID string, now time.Time, limit int) ([]store.FeishuOAuthRefreshAttempt, error)
}

type oauthRefreshRetentionStore interface {
	DeleteTerminalFeishuOAuthRefreshAttempts(accountID string, completedBefore time.Time, limit int) (int64, error)
	CountUnsafeTerminalFeishuOAuthRefreshAttempts(accountID string, completedBefore time.Time) (int64, error)
}

type resourceWorkflowStore interface {
	CreateWorkflowContinuation(store.WorkflowContinuation) (store.WorkflowContinuation, error)
	CancelWorkflowContinuation(requestID, accountID, reason string, now time.Time) error
	StoreWorkflowResult(store.WorkflowResult) (store.WorkflowResult, store.WorkflowContinuation, bool, error)
	ListTerminalWorkflowResultGaps(accountID, kind string, updatedBefore time.Time, limit int) ([]store.WorkflowRequest, error)
}

type resourceCardDeliveryStore interface {
	MarkFeishuCardDeliveryDelivered(accountID, requestID, purpose string, revision int64, now time.Time) error
}

type resourceAccessStore interface {
	resourceMetadataStore
	resourceRequestStore
	resourcePermissionStore
	oauthCredentialStore
	oauthRefreshRetentionStore
	resourceWorkflowStore
	resourceCardDeliveryStore
}

var (
	_ resourceMetadataStore      = (*store.Store)(nil)
	_ resourceRequestStore       = (*store.Store)(nil)
	_ resourcePermissionStore    = (*store.Store)(nil)
	_ oauthCredentialStore       = (*store.Store)(nil)
	_ oauthRefreshRetentionStore = (*store.Store)(nil)
	_ resourceWorkflowStore      = (*store.Store)(nil)
	_ resourceCardDeliveryStore  = (*store.Store)(nil)
	_ resourceAccessStore        = (*store.Store)(nil)
)
