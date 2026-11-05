\set ON_ERROR_STOP on
BEGIN;
CREATE FUNCTION pg_temp.uid(label text) RETURNS uuid LANGUAGE sql IMMUTABLE AS $$ SELECT md5(label)::uuid $$;
CREATE FUNCTION pg_temp.expect_state(statement text, expected text, expected_constraint text DEFAULT NULL, expected_message text DEFAULT NULL) RETURNS void LANGUAGE plpgsql AS $$
DECLARE actual text; actual_constraint text; actual_message text;
BEGIN
  BEGIN
    EXECUTE statement;
  EXCEPTION WHEN OTHERS THEN
    GET STACKED DIAGNOSTICS actual = RETURNED_SQLSTATE, actual_constraint = CONSTRAINT_NAME, actual_message = MESSAGE_TEXT;
    IF actual <> expected THEN RAISE EXCEPTION 'expected SQLSTATE %, got % for %', expected, actual, statement; END IF;
    IF expected_constraint IS NOT NULL AND actual_constraint IS DISTINCT FROM expected_constraint THEN RAISE EXCEPTION 'expected constraint %, got %: %', expected_constraint, actual_constraint, actual_message; END IF;
    IF expected_message IS NOT NULL AND actual_message IS DISTINCT FROM expected_message THEN RAISE EXCEPTION 'expected message %, got %', expected_message, actual_message; END IF;
    RETURN;
  END;
  RAISE EXCEPTION 'statement unexpectedly succeeded: %', statement;
END $$;
CREATE FUNCTION pg_temp.ok(condition boolean, label text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN IF condition IS DISTINCT FROM true THEN RAISE EXCEPTION 'assertion failed: %', label; END IF; END $$;
-- This first real statement must fail with undefined_table on an empty database (RED).
INSERT INTO users(id, username, role) VALUES (pg_temp.uid('owner'), 'owner', 'USER'), (pg_temp.uid('other'), 'other', 'USER'), (pg_temp.uid('admin'), 'admin', 'SUPERADMIN');
INSERT INTO credit_accounts(user_id) SELECT id FROM users;
SELECT pg_temp.ok((SELECT bool_and(available=0 AND frozen=0) FROM credit_accounts), 'zero initial wallets');
SELECT pg_temp.expect_state($q$UPDATE credit_accounts SET available=-1 WHERE user_id=pg_temp.uid('owner')$q$, '23514');
SELECT pg_temp.expect_state($q$UPDATE credit_accounts SET frozen=-1 WHERE user_id=pg_temp.uid('owner')$q$, '23514');
SELECT pg_temp.expect_state($q$INSERT INTO users(id,username,role) VALUES(pg_temp.uid('badname'),'Owner','USER')$q$, '23514');
\echo PASS wallet initialization and nonnegative balances
INSERT INTO device_bindings(id,user_id,public_jwk,key_thumbprint,epoch,status) VALUES(pg_temp.uid('binding'),pg_temp.uid('owner'),'{"kty":"EC","crv":"P-256","x":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","y":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}','thumb',1,'ACTIVE');
SELECT pg_temp.expect_state($q$INSERT INTO device_bindings(id,user_id,public_jwk,key_thumbprint,epoch,status) SELECT pg_temp.uid('binding2'),user_id,public_jwk,'thumb2',1,'ACTIVE' FROM device_bindings$q$,'23505');
SELECT pg_temp.expect_state($q$INSERT INTO device_bindings(id,user_id,public_jwk,key_thumbprint,epoch,status) SELECT pg_temp.uid('privatebinding'),user_id,public_jwk || '{"d":"secret"}'::jsonb,'private',1,'REVOKED' FROM device_bindings$q$,'23514');
INSERT INTO device_sessions(id,user_id,token_hash,binding_id,generation,auth_version,binding_epoch,status,expires_at) VALUES(pg_temp.uid('session'),pg_temp.uid('owner'),decode('01','hex'),pg_temp.uid('binding'),pg_temp.uid('generation'),1,1,'ACTIVE',now()+interval '1 hour');
SELECT pg_temp.expect_state($q$INSERT INTO device_sessions(id,user_id,token_hash,binding_id,generation,auth_version,binding_epoch,status,expires_at) SELECT pg_temp.uid('session2'),user_id,decode('02','hex'),binding_id,pg_temp.uid('g2'),auth_version,binding_epoch,'ACTIVE',expires_at FROM device_sessions$q$,'23505');
SELECT pg_temp.expect_state($q$INSERT INTO device_sessions(id,user_id,token_hash,binding_id,generation,auth_version,binding_epoch,status,expires_at) VALUES(pg_temp.uid('s3'),pg_temp.uid('other'),decode('03','hex'),pg_temp.uid('binding'),pg_temp.uid('g3'),1,1,'ACTIVE',now()+interval '1 hour')$q$,'23503');
SELECT pg_temp.expect_state($q$INSERT INTO device_sessions(id,user_id,token_hash,generation,auth_version,binding_epoch,status,expires_at) VALUES(pg_temp.uid('s4'),pg_temp.uid('other'),decode('04','hex'),pg_temp.uid('g4'),1,1,'ACTIVE',now()+interval '1 hour')$q$,'23514');
\echo PASS active device and session uniqueness, ownership, public-only JWK
INSERT INTO credit_entries(id,account_id,kind,delta_available,delta_frozen,before_available,after_available,before_frozen,after_frozen,operation_id,actor_id,reason) VALUES(pg_temp.uid('entry'),pg_temp.uid('owner'),'GRANT',5,0,0,5,0,0,pg_temp.uid('grant'),pg_temp.uid('admin'),'test grant');
SELECT pg_temp.expect_state($q$INSERT INTO credit_entries SELECT pg_temp.uid('entry2'),account_id,kind,delta_available,delta_frozen,before_available,after_available,before_frozen,after_frozen,operation_id,related_operation_id,actor_id,reason,created_at FROM credit_entries$q$,'23505');
SELECT pg_temp.expect_state($q$UPDATE credit_entries SET reason='rewrite'$q$,'55000');
SELECT pg_temp.expect_state($q$DELETE FROM credit_entries$q$,'55000');
\echo PASS ledger operation deduplication and append-only history
INSERT INTO render_profiles(id,name,manifest,manifest_hash,status) VALUES(pg_temp.uid('profile'),'test','{}','profilehash','ACTIVE');
INSERT INTO documents(id,owner_id,draft_json,revision,render_profile_id,title) VALUES(pg_temp.uid('doc'),pg_temp.uid('owner'),'{}',1,pg_temp.uid('profile'),'test'),(pg_temp.uid('otherdoc'),pg_temp.uid('other'),'{}',1,pg_temp.uid('profile'),'other');
INSERT INTO preview_manifests(id,owner_id,document_id,revision,profile_id,canonical_bytes,content_hash,amounts,expires_at) VALUES(pg_temp.uid('manifest'),pg_temp.uid('owner'),pg_temp.uid('doc'),1,pg_temp.uid('profile'),decode('01','hex'),'content','{}',now()+interval '10 minutes');
-- Same manifest/profile/owner/document/revision, only content_hash differs.
SELECT pg_temp.expect_state($q$INSERT INTO preview_requests(id,owner_id,document_id,revision,profile_id,content_hash,state,manifest_id,expires_at) VALUES(pg_temp.uid('previewmatch'),pg_temp.uid('owner'),pg_temp.uid('doc'),1,pg_temp.uid('profile'),'different-content','READY',pg_temp.uid('manifest'),now()+interval '10 minutes')$q$,'23503','preview_manifest_content_fk');
INSERT INTO preview_requests(id,owner_id,document_id,revision,profile_id,content_hash,state,manifest_id,expires_at) VALUES(pg_temp.uid('previewmatch'),pg_temp.uid('owner'),pg_temp.uid('doc'),1,pg_temp.uid('profile'),'content','READY',pg_temp.uid('manifest'),now()+interval '10 minutes');
SELECT pg_temp.ok((SELECT content_hash='content' FROM preview_requests WHERE id=pg_temp.uid('previewmatch')),'matching READY preview content accepted');
INSERT INTO renderer_manifests(id,image_digest,asset_hashes,algorithm_versions,locale,page_settings,archive_ref) VALUES(pg_temp.uid('renderer'),'sha256:test','{}','{}','es-MX','{}','private/archive');
INSERT INTO document_versions(id,document_id,owner_id,version_no,source_revision,manifest_id,renderer_manifest_id,snapshot_json,unit_cents,qty_micros,subtotal_cents,tax_cents,total_cents) VALUES(pg_temp.uid('version'),pg_temp.uid('doc'),pg_temp.uid('owner'),1,1,pg_temp.uid('manifest'),pg_temp.uid('renderer'),'{}',100,1000000,100,16,116);
SELECT pg_temp.expect_state($q$INSERT INTO document_versions SELECT pg_temp.uid('v2'),document_id,owner_id,2,source_revision,manifest_id,renderer_manifest_id,snapshot_json,unit_cents,qty_micros,subtotal_cents,tax_cents,total_cents,created_at FROM document_versions$q$,'23505');
SELECT pg_temp.expect_state($q$INSERT INTO document_versions SELECT pg_temp.uid('v3'),document_id,owner_id,version_no,2,manifest_id,renderer_manifest_id,snapshot_json,unit_cents,qty_micros,subtotal_cents,tax_cents,total_cents,created_at FROM document_versions$q$,'23505');
SELECT pg_temp.expect_state($q$UPDATE document_versions SET snapshot_json='{"tamper":true}'$q$,'55000');
SELECT pg_temp.expect_state($q$UPDATE preview_manifests SET content_hash='tamper'$q$,'55000');
SELECT pg_temp.expect_state($q$UPDATE render_profiles SET manifest='{"tamper":true}'$q$,'55000');
UPDATE render_profiles SET status='REVOKED';
UPDATE render_profiles SET status='ACTIVE';
SELECT pg_temp.expect_state($q$DELETE FROM renderer_manifests$q$,'55000');
\echo PASS revision/version uniqueness and immutable snapshots/manifests with profile revocation
-- A fresh order may only start RESERVED, attempt 1; version and ownership are valid.
SELECT pg_temp.expect_state($q$INSERT INTO generation_orders(id,version_id,owner_id,cost_points,billing_state,reserve_attempt,deadline) VALUES(pg_temp.uid('order'),pg_temp.uid('version'),pg_temp.uid('owner'),1,'COMPLETED',1,now()+interval '30 minutes')$q$,'23514',NULL,'new order must start RESERVED at attempt 1');
SELECT pg_temp.expect_state($q$INSERT INTO generation_orders(id,version_id,owner_id,cost_points,billing_state,reserve_attempt,deadline) VALUES(pg_temp.uid('order'),pg_temp.uid('version'),pg_temp.uid('owner'),1,'RESERVED',2,now()+interval '30 minutes')$q$,'23514',NULL,'new order must start RESERVED at attempt 1');
INSERT INTO generation_orders(id,version_id,owner_id,cost_points,billing_state,reserve_attempt,deadline) VALUES(pg_temp.uid('order'),pg_temp.uid('version'),pg_temp.uid('owner'),1,'RESERVED',1,now()+interval '30 minutes');
SELECT pg_temp.expect_state($q$INSERT INTO generation_orders SELECT pg_temp.uid('o2'),version_id,owner_id,cost_points,billing_state,reserve_attempt,deadline,created_at,updated_at FROM generation_orders$q$,'23505');
SELECT pg_temp.expect_state($q$UPDATE generation_orders SET owner_id=pg_temp.uid('other')$q$,'55000');
SAVEPOINT retry_state_gate;
UPDATE generation_orders SET billing_state='FAILED_RELEASED' WHERE id=pg_temp.uid('order');
SELECT pg_temp.expect_state($q$UPDATE generation_orders SET billing_state='COMPLETED' WHERE id=pg_temp.uid('order')$q$,'23514',NULL,'invalid order billing transition');
SELECT pg_temp.expect_state($q$UPDATE generation_orders SET billing_state='COMPLETED',reserve_attempt=2 WHERE id=pg_temp.uid('order')$q$,'23514',NULL,'invalid order billing transition');
SELECT pg_temp.expect_state($q$UPDATE generation_orders SET billing_state='RESERVED' WHERE id=pg_temp.uid('order')$q$,'23514',NULL,'retry requires next reserve attempt');
UPDATE generation_orders SET billing_state='RESERVED',reserve_attempt=2 WHERE id=pg_temp.uid('order');
SELECT pg_temp.ok((SELECT billing_state='RESERVED' AND reserve_attempt=2 FROM generation_orders WHERE id=pg_temp.uid('order')),'explicit new-attempt retry accepted');
UPDATE generation_orders SET billing_state='COMPLETED' WHERE id=pg_temp.uid('order');
SELECT pg_temp.ok((SELECT billing_state='COMPLETED' AND reserve_attempt=2 FROM generation_orders WHERE id=pg_temp.uid('order')),'re-reserved attempt may complete');
ROLLBACK TO SAVEPOINT retry_state_gate;
RELEASE SAVEPOINT retry_state_gate;
SELECT pg_temp.expect_state($q$INSERT INTO render_jobs(id,kind,version_id,state,reserve_attempt) VALUES(pg_temp.uid('repair0'),'REPAIR',pg_temp.uid('version'),'QUEUED',1)$q$,'23514');
UPDATE generation_orders SET billing_state='COMPLETED';
-- Completed entitlement exists: only the forbidden paid-order relation differs from the next accepted INSERT.
SELECT pg_temp.expect_state($q$INSERT INTO render_jobs(id,kind,order_id,version_id,state,reserve_attempt) VALUES(pg_temp.uid('repair'),'REPAIR',pg_temp.uid('order'),pg_temp.uid('version'),'QUEUED',1)$q$,'23514','ck_job_order_relation');
INSERT INTO render_jobs(id,kind,version_id,state,reserve_attempt) VALUES(pg_temp.uid('repair'),'REPAIR',pg_temp.uid('version'),'QUEUED',1);
SELECT pg_temp.expect_state($q$INSERT INTO render_jobs(id,kind,version_id,state,reserve_attempt) VALUES(pg_temp.uid('repair2'),'REPAIR',pg_temp.uid('version'),'QUEUED',1)$q$,'23505');
SELECT pg_temp.expect_state($q$INSERT INTO render_jobs(id,kind,order_id,version_id,state,reserve_attempt) VALUES(pg_temp.uid('generatebad'),'GENERATE',pg_temp.uid('order'),pg_temp.uid('missingversion'),'QUEUED',1)$q$,'23503');
\echo PASS order uniqueness, ownership and free repair completed-version requirement
INSERT INTO artifact_objects(id,owner_id,version_id,type,reserve_attempt,attempt_no,storage_key,state,protected_until) VALUES(pg_temp.uid('object'),pg_temp.uid('owner'),pg_temp.uid('version'),'CLEAN_PDF',1,1,'clean/test/one.pdf','UPLOADING',now());
SELECT pg_temp.expect_state($q$INSERT INTO artifact_objects(id,owner_id,version_id,type,reserve_attempt,attempt_no,storage_key,state,protected_until) VALUES(pg_temp.uid('objectbad'),pg_temp.uid('other'),pg_temp.uid('version'),'CLEAN_PDF',1,1,'bad','UPLOADING',now())$q$,'23503');
SELECT pg_temp.expect_state($q$INSERT INTO artifacts(id,version_id,type,current_object_id) VALUES(pg_temp.uid('artifactbad'),pg_temp.uid('version'),'CLEAN_PNG',pg_temp.uid('object'))$q$,'23503');
SELECT pg_temp.expect_state($q$INSERT INTO artifacts(id,version_id,type,current_object_id) VALUES(pg_temp.uid('artifactearly'),pg_temp.uid('version'),'CLEAN_PDF',pg_temp.uid('object'))$q$,'23514');
UPDATE artifact_objects SET object_version='v1',sha256=repeat('a',64),size=100,state='READY' WHERE id=pg_temp.uid('object');
UPDATE artifact_objects SET state='PUBLISHED' WHERE id=pg_temp.uid('object');
INSERT INTO artifacts(id,version_id,type,current_object_id) VALUES(pg_temp.uid('artifact'),pg_temp.uid('version'),'CLEAN_PDF',pg_temp.uid('object'));
SELECT pg_temp.expect_state($q$UPDATE artifact_objects SET storage_key='overwrite'$q$,'55000');
SELECT pg_temp.expect_state($q$UPDATE artifact_objects SET sha256=repeat('b',64)$q$,'55000');
-- PUBLISHED->RETIRED is a legal state transition; only the current reference blocks it.
SELECT pg_temp.expect_state($q$UPDATE artifact_objects SET state='RETIRED' WHERE id=pg_temp.uid('object')$q$,'23514',NULL,'current published reference prevents retirement/GC');
SAVEPOINT current_reference_gate;
DELETE FROM artifacts WHERE id=pg_temp.uid('artifact');
UPDATE artifact_objects SET state='RETIRED' WHERE id=pg_temp.uid('object');
SELECT pg_temp.ok((SELECT state='RETIRED' FROM artifact_objects WHERE id=pg_temp.uid('object')),'retirement accepted after only removing current reference');
ROLLBACK TO SAVEPOINT current_reference_gate;
RELEASE SAVEPOINT current_reference_gate;
INSERT INTO artifact_objects(id,owner_id,version_id,type,reserve_attempt,attempt_no,storage_key,state,protected_until) VALUES(pg_temp.uid('png'),pg_temp.uid('owner'),pg_temp.uid('version'),'CLEAN_PNG',1,1,'clean/test/one.png','UPLOADING',now());
UPDATE artifact_objects SET object_version='v1',sha256=repeat('b',64),size=100,state='READY' WHERE id=pg_temp.uid('png');
UPDATE artifact_objects SET state='PUBLISHED' WHERE id=pg_temp.uid('png');
INSERT INTO artifacts(id,version_id,type,current_object_id) VALUES(pg_temp.uid('pngartifact'),pg_temp.uid('version'),'CLEAN_PNG',pg_temp.uid('png'));
-- Both formats and completed order/hash are valid; missing probe is now the sole failed precondition.
SELECT pg_temp.expect_state($q$INSERT INTO delivery_receipts(version_id,order_id,content_hash) VALUES(pg_temp.uid('version'),pg_temp.uid('order'),'content')$q$,'23514',NULL,'receipt requires completed matching order, content and durability probe');
INSERT INTO durability_probes(order_id,probe_nonce,checked_at) VALUES(pg_temp.uid('order'),pg_temp.uid('probe'),now());
SAVEPOINT receipt_format_gate;
DELETE FROM artifacts WHERE id=pg_temp.uid('pngartifact');
SELECT pg_temp.expect_state($q$INSERT INTO delivery_receipts(version_id,order_id,content_hash) VALUES(pg_temp.uid('version'),pg_temp.uid('order'),'content')$q$,'23514',NULL,'receipt requires both published formal formats');
ROLLBACK TO SAVEPOINT receipt_format_gate;
RELEASE SAVEPOINT receipt_format_gate;
INSERT INTO delivery_receipts(version_id,order_id,content_hash) VALUES(pg_temp.uid('version'),pg_temp.uid('order'),'content');
SELECT pg_temp.expect_state($q$UPDATE delivery_receipts SET content_hash='tamper'$q$,'55000');
\echo PASS artifact identity/purpose/owner, immutable uploaded bytes, GC/current-reference exclusion, probe-before-receipt

SELECT pg_temp.expect_state($q$TRUNCATE credit_entries$q$,'55000');
SELECT pg_temp.expect_state($q$DELETE FROM document_versions$q$,'55000');
SELECT pg_temp.expect_state($q$DELETE FROM preview_manifests$q$,'55000');
SELECT pg_temp.expect_state($q$UPDATE documents SET draft_json='{"changed":true}' WHERE id=pg_temp.uid('doc')$q$,'23514');
UPDATE documents SET draft_json='{"changed":true}',revision=2 WHERE id=pg_temp.uid('doc');
SELECT pg_temp.expect_state($q$UPDATE documents SET revision=1 WHERE id=pg_temp.uid('doc')$q$,'23514');
INSERT INTO idempotency_records(id,actor_id,operation,key_hash,request_hash,status) VALUES(pg_temp.uid('idem'),pg_temp.uid('owner'),'GENERATE',decode('01','hex'),decode('01','hex'),'PENDING');
SELECT pg_temp.expect_state($q$INSERT INTO idempotency_records(id,actor_id,operation,key_hash,request_hash,status) VALUES(pg_temp.uid('idem2'),pg_temp.uid('owner'),'GENERATE',decode('01','hex'),decode('02','hex'),'PENDING')$q$,'23505');
SELECT pg_temp.expect_state($q$INSERT INTO preview_manifests(id,owner_id,document_id,revision,profile_id,canonical_bytes,content_hash,amounts,expires_at) VALUES(pg_temp.uid('badmanifest'),pg_temp.uid('other'),pg_temp.uid('doc'),1,pg_temp.uid('profile'),decode('01','hex'),'bad','{}',now()+interval '10 minutes')$q$,'23503');
SELECT pg_temp.expect_state($q$INSERT INTO document_versions(id,document_id,owner_id,version_no,source_revision,manifest_id,renderer_manifest_id,snapshot_json,unit_cents,qty_micros,subtotal_cents,tax_cents,total_cents) VALUES(pg_temp.uid('wrongtax'),pg_temp.uid('doc'),pg_temp.uid('owner'),2,2,pg_temp.uid('manifest'),pg_temp.uid('renderer'),'{}',100,1000000,100,15,115)$q$,'23514');
\echo PASS history deletion/truncate, revision monotonicity, idempotency and financial arithmetic checks
INSERT INTO mfa_factors(id,user_id,state,factor_version) VALUES(pg_temp.uid('nonefactor'),pg_temp.uid('admin'),'NONE',1);
SELECT pg_temp.expect_state($q$INSERT INTO mfa_factors(id,user_id,state,factor_version) VALUES(pg_temp.uid('nullseed'),pg_temp.uid('admin'),'VERIFIED',2)$q$,'23514');
INSERT INTO mfa_factors(id,user_id,state,seed_ciphertext,factor_version) VALUES(pg_temp.uid('factor'),pg_temp.uid('admin'),'VERIFIED',decode('01','hex'),2);
SELECT pg_temp.expect_state($q$INSERT INTO mfa_factors(id,user_id,state,seed_ciphertext,factor_version) VALUES(pg_temp.uid('factor2'),pg_temp.uid('admin'),'VERIFIED',decode('02','hex'),3)$q$,'23505');
SELECT pg_temp.expect_state($q$INSERT INTO mfa_challenges(id,user_id,context_id,purpose,factor_version,expires_at) VALUES(pg_temp.uid('mfabad'),pg_temp.uid('other'),pg_temp.uid('session'),'STEP_UP',1,now()+interval '1 minute')$q$,'23503');
INSERT INTO device_pairings(id,user_id,public_jwk,key_thumbprint,proof_verified,expires_at) SELECT pg_temp.uid('pairing'),user_id,public_jwk,'candidate',true,now()+interval '15 minutes' FROM device_bindings;
INSERT INTO binding_transfers(id,user_id,pairing_id,target_key_hash,target_epoch,purpose,status,approved_by,verification_ref,expires_at) VALUES(pg_temp.uid('transfer'),pg_temp.uid('owner'),pg_temp.uid('pairing'),'candidate',2,'RESET','PENDING',pg_temp.uid('admin'),'verified support case',now()+interval '10 minutes');
SELECT pg_temp.expect_state($q$INSERT INTO binding_transfers(id,user_id,pairing_id,target_key_hash,target_epoch,purpose,status,approved_by,verification_ref,expires_at) VALUES(pg_temp.uid('transfer2'),pg_temp.uid('owner'),pg_temp.uid('pairing'),'candidate',2,'RESET','PENDING',pg_temp.uid('admin'),'verified support case',now()+interval '10 minutes')$q$,'23505');
SELECT pg_temp.expect_state($q$UPDATE binding_transfers SET target_key_hash='wrong'$q$,'23514');
\echo PASS explicit MFA initialization, verified-factor and transfer uniqueness, cross-user context refusal
INSERT INTO render_jobs(id,kind,order_id,version_id,state,reserve_attempt) VALUES(pg_temp.uid('generate'),'GENERATE',pg_temp.uid('order'),pg_temp.uid('version'),'QUEUED',1);
INSERT INTO scheduler_lanes(id,queue_name) VALUES(pg_temp.uid('lane'),'formal');
INSERT INTO capacity_tickets(id,lane_id,owner_id,job_id,slot_start,slot_end,deadline,status,plan_version) VALUES(pg_temp.uid('ticket'),pg_temp.uid('lane'),pg_temp.uid('owner'),pg_temp.uid('generate'),now(),now()+interval '320 seconds',now()+interval '30 minutes','BOOKED',1);
SELECT pg_temp.expect_state($q$INSERT INTO capacity_tickets(id,lane_id,owner_id,job_id,slot_start,slot_end,deadline,status,plan_version) VALUES(pg_temp.uid('ticket2'),pg_temp.uid('lane'),pg_temp.uid('owner'),pg_temp.uid('repair'),now()+interval '30 seconds',now()+interval '350 seconds',now()+interval '30 minutes','BOOKED',1)$q$,'23514');
SELECT pg_temp.expect_state($q$INSERT INTO capacity_tickets(id,lane_id,owner_id,job_id,slot_start,slot_end,deadline,status,plan_version) VALUES(pg_temp.uid('ticketbad'),pg_temp.uid('lane'),pg_temp.uid('other'),pg_temp.uid('repair'),now()+interval '320 seconds',now()+interval '640 seconds',now()+interval '30 minutes','BOOKED',1)$q$,'23503');
INSERT INTO artifact_read_leases(id,object_id,lease_until) VALUES(pg_temp.uid('readlease'),pg_temp.uid('object'),now()+interval '150 seconds');
-- No reference now, but the live read lease still prevents collection.
DELETE FROM artifacts WHERE id=pg_temp.uid('artifact');
UPDATE artifact_objects SET state='RETIRED' WHERE id=pg_temp.uid('object');
SELECT pg_temp.expect_state($q$UPDATE artifact_objects SET state='GC_CLAIMED' WHERE id=pg_temp.uid('object')$q$,'23514');
SELECT pg_temp.expect_state($q$INSERT INTO artifact_read_leases(id,object_id,lease_until) VALUES(pg_temp.uid('readbad'),pg_temp.uid('png'),now()-interval '1 second')$q$,'23514');
SELECT pg_temp.expect_state($q$INSERT INTO credit_entries(id,account_id,kind,delta_available,delta_frozen,before_available,after_available,before_frozen,after_frozen,operation_id,actor_id,reason) VALUES(pg_temp.uid('adjustbad'),pg_temp.uid('owner'),'ADJUST',1,0,0,1,0,0,pg_temp.uid('adjust'),pg_temp.uid('admin'),'unauthorized adjust')$q$,'23514');
\echo PASS lane overlap/owner, read-lease GC exclusion, unapproved adjustment refusal

INSERT INTO password_tokens(id,token_hash,user_id,purpose,auth_version,expires_at) VALUES(pg_temp.uid('setup_token'),decode('11','hex'),pg_temp.uid('other'),'INITIAL_SETUP',1,now()+interval '15 minutes');
INSERT INTO device_challenges(id,nonce_hash,purpose,subject_id,subject_kind,key_hash,auth_version,epoch,generation,expires_at) VALUES(pg_temp.uid('setup_challenge'),decode('11','hex'),'INITIAL_SETUP',pg_temp.uid('setup_token'),'PASSWORD_TOKEN','candidate',1,1,pg_temp.uid('setup_token'),now()+interval '1 minute');
SELECT pg_temp.expect_state($q$INSERT INTO device_challenges(id,nonce_hash,purpose,subject_id,subject_kind,key_hash,auth_version,epoch,generation,expires_at) VALUES(pg_temp.uid('wrongpurpose'),decode('12','hex'),'LOGIN',pg_temp.uid('setup_token'),'PRE_SESSION','candidate',1,1,pg_temp.uid('setup_token'),now()+interval '1 minute')$q$,'23503');
SELECT pg_temp.expect_state($q$INSERT INTO device_challenges(id,nonce_hash,purpose,subject_id,subject_kind,key_hash,auth_version,epoch,generation,expires_at) VALUES(pg_temp.uid('wrongsubject'),decode('13','hex'),'INITIAL_SETUP',pg_temp.uid('setup_token'),'SESSION','candidate',1,1,pg_temp.uid('setup_token'),now()+interval '1 minute')$q$,'23514');
INSERT INTO auth_pre_sessions(id,token_hash,user_id,purpose,auth_version,binding_epoch,expires_at) VALUES(pg_temp.uid('prelogin'),decode('21','hex'),pg_temp.uid('owner'),'LOGIN',1,1,now()+interval '5 minutes');
INSERT INTO device_challenges(id,nonce_hash,purpose,subject_id,subject_kind,key_hash,auth_version,epoch,generation,expires_at) VALUES(pg_temp.uid('login_challenge'),decode('21','hex'),'LOGIN',pg_temp.uid('prelogin'),'PRE_SESSION','thumb',1,1,pg_temp.uid('prelogin'),now()+interval '1 minute');
SELECT pg_temp.expect_state($q$INSERT INTO device_challenges(id,nonce_hash,purpose,subject_id,subject_kind,key_hash,auth_version,epoch,generation,expires_at) VALUES(pg_temp.uid('wrongkey'),decode('22','hex'),'LOGIN',pg_temp.uid('prelogin'),'PRE_SESSION','wrong',1,1,pg_temp.uid('prelogin'),now()+interval '1 minute')$q$,'23514');
SELECT pg_temp.expect_state($q$INSERT INTO device_challenges(id,nonce_hash,purpose,subject_id,subject_kind,key_hash,auth_version,epoch,generation,expires_at) VALUES(pg_temp.uid('wrongepoch'),decode('23','hex'),'LOGIN',pg_temp.uid('prelogin'),'PRE_SESSION','thumb',1,2,pg_temp.uid('prelogin'),now()+interval '1 minute')$q$,'23503');
INSERT INTO device_challenges(id,nonce_hash,purpose,subject_id,subject_kind,key_hash,auth_version,epoch,generation,expires_at) VALUES(pg_temp.uid('request_challenge'),decode('31','hex'),'REQUEST',pg_temp.uid('session'),'SESSION','thumb',1,1,pg_temp.uid('generation'),now()+interval '1 minute');
SELECT pg_temp.expect_state($q$INSERT INTO device_challenges(id,nonce_hash,purpose,subject_id,subject_kind,key_hash,auth_version,epoch,generation,expires_at) VALUES(pg_temp.uid('wronggeneration'),decode('32','hex'),'REQUEST',pg_temp.uid('session'),'SESSION','thumb',1,1,pg_temp.uid('oldgeneration'),now()+interval '1 minute')$q$,'23514');
UPDATE device_challenges SET used_at=now() WHERE id=pg_temp.uid('request_challenge');
SELECT pg_temp.expect_state($q$UPDATE device_challenges SET used_at=NULL WHERE id=pg_temp.uid('request_challenge')$q$,'55000');
UPDATE password_tokens SET used_at=now() WHERE id=pg_temp.uid('setup_token');
SELECT pg_temp.expect_state($q$UPDATE password_tokens SET used_at=NULL WHERE id=pg_temp.uid('setup_token')$q$,'55000');
SELECT pg_temp.ok((SELECT device_state='NOT_REQUIRED' FROM users WHERE id=pg_temp.uid('admin')),'administrator device state initialization');
\echo PASS challenge subject/purpose/key/epoch/generation, one-way nonce/token consumption and administrator initialization
ROLLBACK;
\echo PASS all database constraint groups (transaction rolled back)
