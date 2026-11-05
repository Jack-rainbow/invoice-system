// GENERATED from api/openapi.json. Do not edit.
// Runtime validation and server authorization remain mandatory.

export type AdminCreditEntry = {
  "after_available": Points;
  "after_frozen": Points;
  "created_at": Timestamp;
  "delta_available": string;
  "delta_frozen": string;
  "id": UUID;
  "kind": "GRANT" | "RESERVE" | "CONSUME" | "RELEASE" | "ADJUST";
  "operation_id": UUID;
  "reason": string;
  "user_id": UUID;
};

export type AdminCreditEntryList = {
  "items": Array<AdminCreditEntry>;
  "next_cursor": string | null;
};

export type AdminUserList = {
  "items": Array<AdminUserSummary>;
  "next_cursor": string | null;
};

export type AdminUserSummary = {
  "binding_epoch": PositiveInt32;
  "device_state": "NOT_REQUIRED" | "INITIAL_PENDING" | "ACTIVE" | "RESET_PENDING";
  "user": UserSummary;
  "wallet": Wallet;
};

export type Amounts = {
  "subtotal": string;
  "tax": string;
  "tax_rate": "0.16";
  "total": string;
};

export type AuditLog = {
  "action": string;
  "actor_id": UUID;
  "created_at": Timestamp;
  "id": UUID;
  "request_id": string;
  "target_id": UUID;
  "target_type": string;
};

export type AuditLogList = {
  "items": Array<AuditLog>;
  "next_cursor": string | null;
};

export type BatchCreateRequest = {
  "accepted_total_cost_points": Points;
  "rows": Array<BatchCreateRow>;
};

export type BatchCreateRow = {
  "document_id": UUID;
  "manifest_id": UUID;
  "revision": PositiveInt32;
};

export type BatchDraftRequest = {
  "base_fields": DraftFields;
  "rows": Array<DraftFieldOverrides>;
};

export type BatchDraftResponse = {
  "id": UUID;
  "rows": Array<BatchDraftRow>;
};

export type BatchDraftRow = {
  "document_id": UUID;
  "revision": PositiveInt32;
  "row_no": number;
};

export type BatchEstimateRequest = {
  "document_ids": Array<UUID>;
};

export type BatchEstimateResponse = {
  "binding": "NON_BINDING";
  "estimated_cost_points": Points;
  "latest_deadline_at": Timestamp;
  "suggested_row_count": number;
};

export type BatchItemResponse = {
  "document_id": UUID;
  "generation": GenerationResponse;
  "row_no": number;
};

export type BatchResponse = {
  "consumed_points": Points;
  "id": UUID;
  "items": Array<BatchItemResponse>;
  "released_points": Points;
  "state": "QUEUED" | "RUNNING" | "COMPLETED" | "PARTIAL_FAILURE" | "FAILED";
};

export type BinaryStream = string;

export type BindChallengeRequest = {
  "purpose": "BIND";
  "transfer_id": UUID;
};

export type BindRequest = {
  "challenge_id": UUID;
  "transfer_id": UUID;
};

export type BindingSummary = {
  "binding_epoch": PositiveInt32;
  "binding_id": string | null;
  "device_state": "NOT_REQUIRED" | "INITIAL_PENDING" | "ACTIVE" | "RESET_PENDING";
  "key_thumbprint": string | null;
  "pairings": Array<PairingResponse>;
};

export type BootstrapConfirmRequest = {
  "bootstrap_context": UUID;
  "totp": string;
};

export type BootstrapConfirmResponse = {
  "expires_at": Timestamp;
  "generation": UUID;
  "recovery_codes": Array<string>;
  "state": "AUTHENTICATED";
};

export type BootstrapStartRequest = {
  "bootstrap_token": string;
  "new_password": string;
};

export type BootstrapStartResponse = {
  "bootstrap_context": UUID;
  "expires_at": Timestamp;
  "otpauth_uri": string;
  "seed": string;
};

export type ChallengeRequest = InitialChallengeRequest | PairChallengeRequest | BindChallengeRequest | SessionChallengeRequest;

export type ChallengeResponse = {
  "binding_epoch": PositiveInt32;
  "challenge_id": UUID;
  "context_id": UUID;
  "expires_at": Timestamp;
  "key_thumbprint": string;
  "nonce": string;
  "proof_key_id": UUID;
  "purpose": "INITIAL_SETUP" | "PAIR" | "BIND" | "LOGIN" | "REQUEST";
  "server_time": Timestamp;
};

export type Count = number;

export type CreateDraftRequest = {
  "fields": DraftFields;
};

export type CreateUserRequest = {
  "customer_verification_ref": string;
  "reason": string;
  "username": string;
};

export type CreateUserResponse = {
  "setup_link": string;
  "user": UserSummary;
  "wallet": Wallet;
};

export type CreditEntry = {
  "after_available": Points;
  "after_frozen": Points;
  "created_at": Timestamp;
  "delta_available": string;
  "delta_frozen": string;
  "id": UUID;
  "kind": "GRANT" | "RESERVE" | "CONSUME" | "RELEASE" | "ADJUST";
  "operation_id": UUID;
  "reason": string;
};

export type CreditEntryList = {
  "items": Array<CreditEntry>;
  "next_cursor": string | null;
};

export type CsrfResponse = {
  "csrf_token": string;
};

export type DeviceContextResponse = {
  "binding_epoch": PositiveInt32;
  "binding_id": string | null;
  "generation": UUID;
  "key_thumbprint": string | null;
  "server_time": Timestamp;
};

export type DocumentList = {
  "items": Array<DocumentSummary>;
  "next_cursor": string | null;
};

export type DocumentSummary = {
  "created_at": Timestamp;
  "id": UUID;
  "revision": PositiveInt32;
  "status": "DRAFT" | "GENERATING" | "HAS_VERSIONS";
};

export type DownloadPaths = {
  "pdf": string;
  "png": string;
};

export type DraftFieldOverrides = {
  "certification_display_at"?: string | null;
  "description"?: string;
  "issued_at"?: Timestamp;
  "product_code"?: string;
  "quantity"?: string;
  "recipient_name"?: string;
  "unit_code"?: string;
  "unit_description"?: string;
  "unit_price"?: string;
};

export type DraftFields = {
  "certification_display_at": string | null;
  "description": string;
  "issued_at": Timestamp;
  "product_code": string;
  "quantity": string;
  "recipient_name": string;
  "unit_code": string;
  "unit_description": string;
  "unit_price": string;
};

export type DraftResponse = {
  "amounts": Amounts;
  "calculation_version": "mxn-iva16-halfup-v1";
  "fields": DraftFields;
  "id": UUID;
  "render_profile_id": UUID;
  "revision": PositiveInt32;
};

export type EmptyRequest = Record<string, never>;

export type Error = {
  "code": "VALIDATION_ERROR" | "TEXT_DOES_NOT_FIT" | "DEVICE_NONCE_EXPIRED" | "DEVICE_NONCE_REPLAY" | "DEVICE_PROOF_INVALID" | "SESSION_EXPIRED" | "SESSION_REPLACED" | "FORBIDDEN" | "DEVICE_MISMATCH" | "DEVICE_PROOF_REQUIRED" | "NOT_FOUND" | "REVISION_CONFLICT" | "DOCUMENT_HAS_VERSIONS" | "PREVIEW_STALE" | "PROFILE_REVOKED" | "CREDITS_INSUFFICIENT" | "COST_CHANGED" | "IDEMPOTENCY_CONFLICT" | "GENERATION_IN_PROGRESS" | "DEVICE_BINDING_CHANGED" | "DEVICE_TRANSFER_REQUIRED" | "DEVICE_TRANSFER_EXPIRED" | "BATCH_CAPACITY_EXCEEDED" | "ARTIFACT_REPAIRING" | "RATE_LIMITED" | "RESOURCE_QUOTA_EXCEEDED" | "RESULT_PENDING" | "RECOVERY_HOLD" | "DEPENDENCY_UNAVAILABLE" | "AUTHENTICATION_REQUIRED" | "CSRF_INVALID" | "ORIGIN_REJECTED" | "INVALID_REQUEST" | "SETUP_LINK_ALREADY_ISSUED" | "AMOUNT_LIMIT";
  "details"?: ErrorDetails;
  "message": string;
  "request_id": string;
};

export type ErrorDetails = {
  "current_revision"?: PositiveInt32;
  "field_errors"?: Array<FieldError>;
  "generation_id"?: UUID;
  "operation_id"?: UUID;
  "retry_after_seconds"?: Count;
};

export type FactorChangedResponse = {
  "state": "FACTOR_CHANGED_REAUTHENTICATE";
};

export type FieldBox = {
  "h": number;
  "w": number;
  "x": number;
  "y": number;
};

export type FieldBoxMap = {
  "certification_display_at"?: FieldBox;
  "description"?: FieldBox;
  "issued_at"?: FieldBox;
  "line_amount"?: FieldBox;
  "product_code"?: FieldBox;
  "quantity"?: FieldBox;
  "recipient_name"?: FieldBox;
  "subtotal"?: FieldBox;
  "tax_amount"?: FieldBox;
  "tax_detail"?: FieldBox;
  "tax_label"?: FieldBox;
  "total"?: FieldBox;
  "total_words"?: FieldBox;
  "unit_code"?: FieldBox;
  "unit_description"?: FieldBox;
  "unit_price"?: FieldBox;
};

export type FieldError = {
  "code": "REQUIRED" | "INVALID_FORMAT" | "OUT_OF_RANGE" | "TEXT_DOES_NOT_FIT";
  "field": string;
  "message": string;
};

export type GenerateRequest = {
  "accepted_cost_points": "0" | "1";
  "accepted_preview_manifest_id": UUID;
  "expected_revision": PositiveInt32;
};

export type GenerationCompletedResponse = {
  "available_points": Points;
  "billing_state": "COMPLETED";
  "cost_points": "0" | "1";
  "deadline_at": Timestamp;
  "delivery_state": "READY";
  "downloads": DownloadPaths;
  "frozen_points": Points;
  "generation_id": UUID;
  "state": "COMPLETED";
  "version_id": UUID;
};

export type GenerationFailedResponse = {
  "available_points": Points;
  "billing_state": "FAILED_RELEASED";
  "cost_points": "0" | "1";
  "deadline_at": Timestamp;
  "delivery_state": "PENDING";
  "frozen_points": Points;
  "generation_id": UUID;
  "state": "FAILED";
  "version_id": UUID;
};

export type GenerationFinalizingResponse = {
  "available_points": Points;
  "billing_state": "COMPLETED";
  "cost_points": "0" | "1";
  "deadline_at": Timestamp;
  "delivery_state": "PENDING";
  "frozen_points": Points;
  "generation_id": UUID;
  "state": "FINALIZING";
  "version_id": UUID;
};

export type GenerationPendingResponse = {
  "available_points": Points;
  "billing_state": "RESERVED";
  "cost_points": "0" | "1";
  "deadline_at": Timestamp;
  "delivery_state": "PENDING";
  "frozen_points": Points;
  "generation_id": UUID;
  "state": "QUEUED" | "RUNNING" | "RETRY_WAIT";
  "version_id": UUID;
};

export type GenerationResponse = GenerationPendingResponse | GenerationFinalizingResponse | GenerationCompletedResponse | GenerationFailedResponse;

export type GrantRequest = {
  "amount": string;
  "reason": string;
};

export type GrantResponse = {
  "operation_id": UUID;
  "wallet": Wallet;
};

export type HealthResponse = {
  "status": "ok";
};

export type InfrastructureError = {
  "code": "DEPENDENCY_UNAVAILABLE" | "NOT_FOUND" | "METHOD_NOT_ALLOWED";
  "request_id"?: string;
};

export type InitialChallengeRequest = {
  "public_jwk": PublicJWK;
  "purpose": "INITIAL_SETUP";
  "setup_token": string;
};

export type InitialPasswordSetRequest = {
  "challenge_id": UUID;
  "new_password": string;
  "public_jwk": PublicJWK;
  "token": string;
};

export type InitialPasswordSetResponse = {
  "context_id": UUID;
  "expires_at": Timestamp;
  "state": "BIND_REQUIRED";
  "transfer_id": UUID;
};

export type KeyThumbprint = string;

export type LoginDeviceRequest = {
  "challenge_id": UUID;
};

export type LoginRequest = {
  "password": string;
  "username": string;
};

export type MFAChallengeRequest = {
  "purpose": "LOGIN" | "STEP_UP" | "ENROLL_CONFIRM";
};

export type MFAChallengeResponse = {
  "challenge_id": UUID;
  "expires_at": Timestamp;
};

export type MFAEnrollmentConfirmRequest = {
  "challenge_id": UUID;
  "totp": string;
};

export type MFAEnrollmentResponse = {
  "enrollment_id": UUID;
  "expires_at": Timestamp;
  "otpauth_uri": string;
  "seed": string;
};

export type MFARecoveryRequest = {
  "challenge_id": UUID;
  "recovery_code": string;
};

export type MFATotpRequest = {
  "challenge_id": UUID;
  "totp": string;
};

export type MFAVerifyRequest = MFATotpRequest | MFARecoveryRequest;

export type MeResponse = {
  "generation": UUID;
  "user": UserSummary;
  "wallet": Wallet;
};

export type NotReadyResponse = {
  "request_id": string;
  "status": "not_ready";
};

export type OperationLookupRequest = {
  "idempotency_key": UUID;
  "operation": "CREATE_USER" | "CREDIT_GRANT" | "DEVICE_RESET" | "GENERATE" | "CREATE_BATCH" | "CREATE_BATCH_DRAFT" | "REISSUE_SETUP" | "REISSUE_PASSWORD_RESET";
};

export type OperationLookupResponse = {
  "generation"?: GenerationResponse;
  "operation": "CREATE_USER" | "CREDIT_GRANT" | "DEVICE_RESET" | "GENERATE" | "CREATE_BATCH" | "CREATE_BATCH_DRAFT" | "REISSUE_SETUP" | "REISSUE_PASSWORD_RESET";
  "operation_id": UUID;
  "resource_id": string | null;
  "status": "PENDING" | "COMPLETED" | "FAILED";
};

export type PairChallengeRequest = {
  "public_jwk": PublicJWK;
  "purpose": "PAIR";
};

export type PairingRequest = {
  "challenge_id": UUID;
  "public_jwk": PublicJWK;
};

export type PairingResponse = {
  "expires_at": Timestamp;
  "key_thumbprint": KeyThumbprint;
  "pairing_id": UUID;
};

export type PasswordChangeRequest = {
  "new_password": string;
  "old_password": string;
};

export type PasswordResetResponse = {
  "operation_id": UUID;
  "reset_link": string;
};

export type PasswordSetRequest = InitialPasswordSetRequest | ResetPasswordSetRequest;

export type PasswordSetResponse = InitialPasswordSetResponse | ResetPasswordSetResponse;

export type Points = string;

export type PositiveInt32 = number;

export type PreSessionResponse = {
  "context_id": UUID;
  "expires_at": Timestamp;
  "stage": "DEVICE_REQUIRED" | "MFA_REQUIRED" | "TRANSFER_REQUIRED" | "AUTHENTICATED";
};

export type PresetList = {
  "items": Array<PresetResponse>;
  "next_cursor": string | null;
};

export type PresetRequest = {
  "fields": DraftFields;
  "name": string;
};

export type PresetResponse = {
  "fields": DraftFields;
  "id": UUID;
  "name": string;
};

export type PreviewFailed = {
  "draft_revision": PositiveInt32;
  "error_code": "TEXT_DOES_NOT_FIT" | "DEPENDENCY_UNAVAILABLE" | "PREVIEW_STALE" | "PROFILE_REVOKED";
  "id": UUID;
  "state": "FAILED" | "EXPIRED";
};

export type PreviewPending = {
  "draft_revision": PositiveInt32;
  "id": UUID;
  "state": "QUEUED" | "RUNNING";
};

export type PreviewReady = {
  "confirmation_expires_at": Timestamp;
  "draft_revision": PositiveInt32;
  "id": UUID;
  "image_path": string;
  "preview_manifest_id": UUID;
  "state": "READY";
};

export type PreviewResponse = PreviewPending | PreviewReady | PreviewFailed;

export type ProfileChangeRequest = {
  "expected_revision": PositiveInt32;
  "target_profile_id": UUID;
};

export type PublicJWK = {
  "crv": "P-256";
  "kty": "EC";
  "x": string;
  "y": string;
};

export type ReadinessFailure = NotReadyResponse | InfrastructureError;

export type ReadyResponse = {
  "status": "ready";
};

export type ReasonRequest = {
  "reason": string;
};

export type RenderProfileList = {
  "field_boxes"?: FieldBoxMap;
  "items": Array<RenderProfileSummary>;
};

export type RenderProfileSummary = {
  "field_boxes"?: FieldBoxMap;
  "id": UUID;
  "name": string;
  "status": "ACTIVE" | "REVOKED";
};

export type ResetPasswordSetRequest = {
  "new_password": string;
  "token": string;
};

export type ResetPasswordSetResponse = {
  "state": "PASSWORD_RESET_COMPLETE";
};

export type RevisionRequest = {
  "expected_revision": PositiveInt32;
};

export type SaveDraftRequest = {
  "expected_revision": PositiveInt32;
  "fields": DraftFields;
};

export type SessionChallengeRequest = {
  "purpose": "LOGIN" | "REQUEST";
};

export type SessionResponse = {
  "expires_at": Timestamp;
  "generation": UUID;
  "state": "AUTHENTICATED";
};

export type StatusRequest = {
  "is_active": boolean;
  "reason": string;
};

export type StepUpResponse = {
  "recent_verified_at": Timestamp;
};

export type Timestamp = string;

export type TransferApprovalRequest = {
  "expected_binding_epoch": PositiveInt32;
  "pairing_id": UUID;
  "reason": string;
  "target_key_hash": KeyThumbprint;
  "verification_ref": string;
};

export type TransferApprovalResponse = {
  "expires_at": Timestamp;
  "target_epoch": PositiveInt32;
  "transfer_id": UUID;
};

export type TransferCancelRequest = {
  "expected_binding_epoch": PositiveInt32;
  "reason": string;
};

export type TransferCancelResponse = {
  "binding_epoch": PositiveInt32;
  "state": "CANCELLED";
};

export type TransferResponse = {
  "expires_at": Timestamp;
  "state": "PENDING" | "CONSUMED" | "CANCELLED" | "EXPIRED";
  "target_epoch": PositiveInt32;
  "target_key_hash": KeyThumbprint;
  "transfer_id": UUID;
};

export type UUID = string;

export type UserSummary = {
  "id": UUID;
  "is_active": boolean;
  "onboarding_state": "NEEDS_SETUP" | "ENROLLMENT_REQUIRED" | "READY";
  "role": "USER" | "SUPERADMIN";
  "username": string;
};

export type VersionList = {
  "items": Array<VersionSummary>;
  "next_cursor": string | null;
};

export type VersionSummary = {
  "amounts": Amounts;
  "created_at": Timestamp;
  "delivery_state": "PENDING" | "READY";
  "document_id": UUID;
  "generation_id": UUID;
  "id": UUID;
  "source_revision": PositiveInt32;
  "version_no": PositiveInt32;
};

export type Wallet = {
  "available": Points;
  "frozen": Points;
};
