package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

var (
	lightsailIdempotencyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,199}$`)
	lightsailRegionPattern      = regexp.MustCompile(`^[a-z]{2}(?:-[a-z]+)+-[0-9]+$`)
	lightsailZonePattern        = regexp.MustCompile(`^[a-z]{2}(?:-[a-z]+)+-[0-9]+[a-z]$`)
	lightsailInstancePattern    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,61}[A-Za-z0-9_]$`)
)

type LightsailCatalogSelection struct {
	Region           string `json:"region"`
	AvailabilityZone string `json:"availabilityZone"`
	BlueprintID      string `json:"blueprintId"`
	BundleID         string `json:"bundleId"`
}

type LightsailCreateInput struct {
	Name            string                    `json:"name"`
	AWSInstanceName string                    `json:"awsInstanceName"`
	Selection       LightsailCatalogSelection `json:"selection"`
	Script          string                    `json:"script"`
}

type LightsailInstance struct {
	ID                  string                    `json:"id"`
	Name                string                    `json:"name"`
	Selection           LightsailCatalogSelection `json:"selection"`
	AWSInstanceName     string                    `json:"awsInstanceName"`
	AWSARN              string                    `json:"awsArn,omitempty"`
	StaticIPName        string                    `json:"staticIpName"`
	StaticIPAddress     string                    `json:"staticIpAddress,omitempty"`
	AWSState            string                    `json:"awsState"`
	LifecycleState      string                    `json:"lifecycleState"`
	DeploymentState     string                    `json:"deploymentState"`
	ScriptSHA256        string                    `json:"scriptSha256"`
	ScriptSize          int                       `json:"scriptSize"`
	ExecutionStartedAt  *time.Time                `json:"executionStartedAt,omitempty"`
	HostKeySHA256       []string                  `json:"hostKeySha256"`
	ExecutionOutput     string                    `json:"executionOutput"`
	ExitCode            *int                      `json:"exitCode,omitempty"`
	ErrorCode           string                    `json:"errorCode"`
	Revision            int64                     `json:"revision"`
	CreatedAt           time.Time                 `json:"createdAt"`
	UpdatedAt           time.Time                 `json:"updatedAt"`
	DestroyedAt         *time.Time                `json:"destroyedAt,omitempty"`
	CreateOperationID   string                    `json:"-"`
	FirewallOperationID string                    `json:"-"`
	StaticIPOperationID string                    `json:"-"`
	StopOperationID     string                    `json:"-"`
	DeleteOperationID   string                    `json:"-"`
}

type LightsailJob struct {
	ID                  string            `json:"id"`
	InstanceID          string            `json:"instanceId"`
	Kind                string            `json:"kind"`
	State               string            `json:"state"`
	Stage               string            `json:"stage"`
	LeaseToken          string            `json:"-"`
	LeaseExpiresAt      *time.Time        `json:"-"`
	Attempts            int               `json:"attempts"`
	NextAttemptAt       time.Time         `json:"nextAttemptAt"`
	ProviderOperationID string            `json:"-"`
	ErrorCode           string            `json:"errorCode"`
	CreatedAt           time.Time         `json:"createdAt"`
	UpdatedAt           time.Time         `json:"updatedAt"`
	CompletedAt         *time.Time        `json:"completedAt,omitempty"`
	Instance            LightsailInstance `json:"-"`
}

type LightsailExecution struct {
	Instance LightsailInstance
	Script   string
}

type LightsailExecutionResult struct {
	State         string
	Output        string
	ExitCode      *int
	HostKeySHA256 []string
	ErrorCode     string
}

type LightsailInstanceUpdate struct {
	AWSARN              *string
	AWSState            *string
	StaticIPAddress     *string
	LifecycleState      *string
	CreateOperationID   *string
	FirewallOperationID *string
	StaticIPOperationID *string
	StopOperationID     *string
	DeleteOperationID   *string
}

type lightsailScanner interface{ Scan(...any) error }

const lightsailInstanceColumns = `id::text,name,region,availability_zone,blueprint_id,bundle_id,aws_instance_name,aws_arn,static_ip_name,COALESCE(host(static_ip_address),''),aws_state,lifecycle_state,deployment_state,script_sha256,script_size,execution_started_at,host_key_sha256,execution_output,exit_code,error_code,revision,created_at,updated_at,destroyed_at,create_operation_id,firewall_operation_id,static_ip_operation_id,stop_operation_id,delete_operation_id`
const lightsailJobColumns = `id::text,instance_id::text,kind,state,stage,COALESCE(lease_token::text,''),lease_expires_at,attempts,next_attempt_at,provider_operation_id,error_code,created_at,updated_at,completed_at`

func scanLightsailInstance(row lightsailScanner) (LightsailInstance, error) {
	var value LightsailInstance
	err := row.Scan(&value.ID, &value.Name, &value.Selection.Region, &value.Selection.AvailabilityZone, &value.Selection.BlueprintID, &value.Selection.BundleID, &value.AWSInstanceName, &value.AWSARN, &value.StaticIPName, &value.StaticIPAddress, &value.AWSState, &value.LifecycleState, &value.DeploymentState, &value.ScriptSHA256, &value.ScriptSize, &value.ExecutionStartedAt, &value.HostKeySHA256, &value.ExecutionOutput, &value.ExitCode, &value.ErrorCode, &value.Revision, &value.CreatedAt, &value.UpdatedAt, &value.DestroyedAt, &value.CreateOperationID, &value.FirewallOperationID, &value.StaticIPOperationID, &value.StopOperationID, &value.DeleteOperationID)
	if value.HostKeySHA256 == nil {
		value.HostKeySHA256 = []string{}
	}
	return value, err
}

func scanLightsailJob(row lightsailScanner) (LightsailJob, error) {
	var value LightsailJob
	err := row.Scan(&value.ID, &value.InstanceID, &value.Kind, &value.State, &value.Stage, &value.LeaseToken, &value.LeaseExpiresAt, &value.Attempts, &value.NextAttemptAt, &value.ProviderOperationID, &value.ErrorCode, &value.CreatedAt, &value.UpdatedAt, &value.CompletedAt)
	return value, err
}

func validLightsailCreateInput(key string, input LightsailCreateInput) bool {
	name := strings.TrimSpace(input.Name)
	selection := input.Selection
	return lightsailIdempotencyPattern.MatchString(key) && name == input.Name && len(name) >= 1 && len(name) <= 64 && utf8.ValidString(name) &&
		lightsailInstancePattern.MatchString(input.AWSInstanceName) &&
		lightsailRegionPattern.MatchString(selection.Region) && lightsailZonePattern.MatchString(selection.AvailabilityZone) && strings.HasPrefix(selection.AvailabilityZone, selection.Region) &&
		len(selection.BlueprintID) >= 1 && len(selection.BlueprintID) <= 255 && len(selection.BundleID) >= 1 && len(selection.BundleID) <= 255 &&
		utf8.ValidString(input.Script) && len(input.Script) <= 65536
}

func lightsailRequestHash(input LightsailCreateInput, scriptHash string) string {
	payload, _ := json.Marshal(struct {
		Name            string
		AWSInstanceName string
		Selection       LightsailCatalogSelection
		ScriptHash      string
		ScriptSize      int
	}{input.Name, input.AWSInstanceName, input.Selection, scriptHash, len(input.Script)})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func (s *Store) AdminCreateLightsailInstance(ctx context.Context, who Identity, idempotencyKey string, input LightsailCreateInput) (LightsailInstance, error) {
	if !validLightsailCreateInput(idempotencyKey, input) {
		return LightsailInstance{}, ErrInvalid
	}
	scriptDigest := sha256.Sum256([]byte(input.Script))
	scriptHash := hex.EncodeToString(scriptDigest[:])
	requestHash := lightsailRequestHash(input, scriptHash)
	tx, err := s.adminTx(ctx, who)
	if err != nil {
		return LightsailInstance{}, err
	}
	defer tx.Rollback(ctx)
	existing, err := scanLightsailInstance(tx.QueryRow(ctx, `SELECT `+lightsailInstanceColumns+` FROM lightsail_instances WHERE created_by=$1 AND idempotency_key=$2 FOR UPDATE`, who.MemberID, idempotencyKey))
	if err == nil {
		var storedHash string
		if err = tx.QueryRow(ctx, "SELECT request_hash FROM lightsail_instances WHERE id=$1", existing.ID).Scan(&storedHash); err != nil {
			return LightsailInstance{}, err
		}
		if storedHash != requestHash {
			return LightsailInstance{}, ErrConflict
		}
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return LightsailInstance{}, err
	}
	id := newID()
	suffix := strings.ReplaceAll(id, "-", "")[:12]
	awsName := input.AWSInstanceName
	staticName := "reladock-ip-" + suffix
	deploymentState := "pending"
	var scriptBody *string
	if input.Script == "" {
		deploymentState = "not-requested"
	} else {
		scriptBody = &input.Script
	}
	created, err := scanLightsailInstance(tx.QueryRow(ctx, `INSERT INTO lightsail_instances(
 id,created_by,idempotency_key,request_hash,name,region,availability_zone,blueprint_id,bundle_id,aws_instance_name,static_ip_name,lifecycle_state,deployment_state,script_sha256,script_size,script_body
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'requested',$12,$13,$14,$15) RETURNING `+lightsailInstanceColumns,
		id, who.MemberID, idempotencyKey, requestHash, input.Name, input.Selection.Region, input.Selection.AvailabilityZone, input.Selection.BlueprintID, input.Selection.BundleID, awsName, staticName, deploymentState, scriptHash, len(input.Script), scriptBody))
	if err != nil {
		return LightsailInstance{}, nodeError(err)
	}
	jobID := newID()
	if _, err = tx.Exec(ctx, `INSERT INTO lightsail_jobs(id,instance_id,kind,state,stage) VALUES($1,$2,'create','queued','reconcile-instance')`, jobID, id); err != nil {
		return LightsailInstance{}, err
	}
	if err = platformLog(ctx, tx, &who, "lightsail.create-requested", id, map[string]any{"name": input.Name, "region": input.Selection.Region, "awsInstanceName": awsName, "scriptSha256": scriptHash, "scriptSize": len(input.Script), "jobId": jobID}); err != nil {
		return LightsailInstance{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return LightsailInstance{}, err
	}
	return created, nil
}

func (s *Store) AdminLightsailInstances(ctx context.Context, who Identity) ([]LightsailInstance, error) {
	tx, err := s.adminReadTx(ctx, who)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT `+lightsailInstanceColumns+` FROM lightsail_instances ORDER BY created_at DESC,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []LightsailInstance{}
	for rows.Next() {
		value, scanErr := scanLightsailInstance(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) AdminRequestLightsailStop(ctx context.Context, who Identity, id string, revision int64) (LightsailJob, error) {
	if !upgradeIDPattern.MatchString(id) || revision < 1 {
		return LightsailJob{}, ErrInvalid
	}
	tx, err := s.adminTx(ctx, who)
	if err != nil {
		return LightsailJob{}, err
	}
	defer tx.Rollback(ctx)
	instance, err := scanLightsailInstance(tx.QueryRow(ctx, `SELECT `+lightsailInstanceColumns+` FROM lightsail_instances WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return LightsailJob{}, ErrNotFound
	}
	if err != nil {
		return LightsailJob{}, err
	}
	existing, jobErr := activeLightsailJob(ctx, tx, id)
	if jobErr == nil {
		if existing.Kind == "stop" {
			return existing, tx.Commit(ctx)
		}
		return LightsailJob{}, ErrConflict
	}
	if !errors.Is(jobErr, pgx.ErrNoRows) {
		return LightsailJob{}, jobErr
	}
	if instance.Revision != revision || instance.AWSARN == "" || instance.LifecycleState == "destroyed" || instance.LifecycleState == "destroying" {
		return LightsailJob{}, ErrConflict
	}
	job, err := scanLightsailJob(tx.QueryRow(ctx, `INSERT INTO lightsail_jobs(id,instance_id,kind,state,stage) VALUES($1,$2,'stop','queued','reconcile-stop') RETURNING `+lightsailJobColumns, newID(), id))
	if err != nil {
		return LightsailJob{}, nodeError(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE lightsail_instances SET lifecycle_state='stopping',revision=revision+1,updated_at=clock_timestamp() WHERE id=$1`, id); err != nil {
		return LightsailJob{}, err
	}
	if err = platformLog(ctx, tx, &who, "lightsail.stop-requested", id, map[string]any{"jobId": job.ID, "revision": revision}); err != nil {
		return LightsailJob{}, err
	}
	return job, tx.Commit(ctx)
}

func (s *Store) AdminRequestLightsailDestroy(ctx context.Context, who Identity, id string, revision int64, confirmation string) (LightsailJob, error) {
	if !upgradeIDPattern.MatchString(id) || revision < 1 {
		return LightsailJob{}, ErrInvalid
	}
	tx, err := s.adminTx(ctx, who)
	if err != nil {
		return LightsailJob{}, err
	}
	defer tx.Rollback(ctx)
	instance, err := scanLightsailInstance(tx.QueryRow(ctx, `SELECT `+lightsailInstanceColumns+` FROM lightsail_instances WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return LightsailJob{}, ErrNotFound
	}
	if err != nil {
		return LightsailJob{}, err
	}
	if confirmation != instance.AWSInstanceName {
		return LightsailJob{}, ErrConflict
	}
	existing, jobErr := activeLightsailJob(ctx, tx, id)
	if jobErr == nil {
		if existing.Kind == "destroy" {
			return existing, tx.Commit(ctx)
		}
		return LightsailJob{}, ErrConflict
	}
	if !errors.Is(jobErr, pgx.ErrNoRows) {
		return LightsailJob{}, jobErr
	}
	if instance.Revision != revision || instance.AWSState != "stopped" || instance.LifecycleState != "stopped" || instance.AWSARN == "" || instance.StaticIPAddress == "" || instance.StaticIPName == "" {
		return LightsailJob{}, ErrConflict
	}
	job, err := scanLightsailJob(tx.QueryRow(ctx, `INSERT INTO lightsail_jobs(id,instance_id,kind,state,stage) VALUES($1,$2,'destroy','queued','reconcile-ownership') RETURNING `+lightsailJobColumns, newID(), id))
	if err != nil {
		return LightsailJob{}, nodeError(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE lightsail_instances SET lifecycle_state='destroying',revision=revision+1,updated_at=clock_timestamp() WHERE id=$1`, id); err != nil {
		return LightsailJob{}, err
	}
	if err = platformLog(ctx, tx, &who, "lightsail.destroy-requested", id, map[string]any{"jobId": job.ID, "revision": revision, "awsInstanceName": instance.AWSInstanceName, "staticIpName": instance.StaticIPName}); err != nil {
		return LightsailJob{}, err
	}
	return job, tx.Commit(ctx)
}

func activeLightsailJob(ctx context.Context, tx pgx.Tx, instanceID string) (LightsailJob, error) {
	return scanLightsailJob(tx.QueryRow(ctx, `SELECT `+lightsailJobColumns+` FROM lightsail_jobs WHERE instance_id=$1 AND state IN ('queued','leased','retrying') FOR UPDATE`, instanceID))
}

func (s *Store) ClaimLightsailJob(ctx context.Context, leaseDuration time.Duration) (*LightsailJob, error) {
	if leaseDuration < time.Second || leaseDuration > 15*time.Minute {
		return nil, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `WITH crossed AS (
 SELECT j.id,j.instance_id FROM lightsail_jobs j JOIN lightsail_instances i ON i.id=j.instance_id
 WHERE j.state='leased' AND j.lease_expires_at<=clock_timestamp() AND i.execution_started_at IS NOT NULL AND i.deployment_state='running'
 FOR UPDATE OF j,i SKIP LOCKED
) UPDATE lightsail_jobs j SET state='unknown',error_code='execution-outcome-unknown',lease_token=NULL,lease_expires_at=NULL,completed_at=clock_timestamp(),updated_at=clock_timestamp()
 FROM crossed c WHERE j.id=c.id`); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `UPDATE lightsail_instances i SET deployment_state='unknown',error_code='execution-outcome-unknown',updated_at=clock_timestamp()
 WHERE i.deployment_state='running' AND EXISTS(SELECT 1 FROM lightsail_jobs j WHERE j.instance_id=i.id AND j.state='unknown' AND j.error_code='execution-outcome-unknown')`); err != nil {
		return nil, err
	}
	job, err := scanLightsailJob(tx.QueryRow(ctx, `SELECT `+lightsailJobColumns+` FROM lightsail_jobs
 WHERE next_attempt_at<=clock_timestamp() AND (state IN ('queued','retrying') OR (state='leased' AND lease_expires_at<=clock_timestamp()))
 ORDER BY next_attempt_at,created_at,id FOR UPDATE SKIP LOCKED LIMIT 1`))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	token := newID()
	expires := time.Now().UTC().Add(leaseDuration)
	job, err = scanLightsailJob(tx.QueryRow(ctx, `UPDATE lightsail_jobs SET state='leased',lease_token=$2,lease_expires_at=$3,attempts=attempts+1,error_code='',updated_at=clock_timestamp() WHERE id=$1 RETURNING `+lightsailJobColumns, job.ID, token, expires))
	if err != nil {
		return nil, err
	}
	job.Instance, err = scanLightsailInstance(tx.QueryRow(ctx, `SELECT `+lightsailInstanceColumns+` FROM lightsail_instances WHERE id=$1`, job.InstanceID))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &job, nil
}

func (s *Store) UpdateLightsailInstance(ctx context.Context, jobID, leaseToken, expectedStage string, update LightsailInstanceUpdate) error {
	if !upgradeIDPattern.MatchString(jobID) || !upgradeIDPattern.MatchString(leaseToken) || expectedStage == "" {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var instanceID string
	err = tx.QueryRow(ctx, `SELECT instance_id::text FROM lightsail_jobs WHERE id=$1 AND state='leased' AND lease_token=$2 AND lease_expires_at>clock_timestamp() AND stage=$3 FOR UPDATE`, jobID, leaseToken, expectedStage).Scan(&instanceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE lightsail_instances SET
 aws_arn=COALESCE($2,aws_arn),
 aws_state=COALESCE($3,aws_state),
 static_ip_address=CASE WHEN $4::text IS NULL THEN static_ip_address ELSE NULLIF($4,'')::inet END,
 lifecycle_state=COALESCE($5,lifecycle_state),
 create_operation_id=COALESCE($6,create_operation_id),
 firewall_operation_id=COALESCE($7,firewall_operation_id),
 static_ip_operation_id=COALESCE($8,static_ip_operation_id),
 stop_operation_id=COALESCE($9,stop_operation_id),
 delete_operation_id=COALESCE($10,delete_operation_id),
 updated_at=clock_timestamp()
 WHERE id=$1`, instanceID, update.AWSARN, update.AWSState, update.StaticIPAddress, update.LifecycleState, update.CreateOperationID, update.FirewallOperationID, update.StaticIPOperationID, update.StopOperationID, update.DeleteOperationID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) AdvanceLightsailJob(ctx context.Context, jobID, leaseToken, expectedStage, nextStage, providerOperationID string) error {
	if !upgradeIDPattern.MatchString(jobID) || !upgradeIDPattern.MatchString(leaseToken) || expectedStage == "" || nextStage == "" || len(nextStage) > 80 || len(providerOperationID) > 255 {
		return ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `UPDATE lightsail_jobs SET stage=$4,provider_operation_id=$5,updated_at=clock_timestamp()
 WHERE id=$1 AND state='leased' AND lease_token=$2 AND lease_expires_at>clock_timestamp() AND stage=$3`, jobID, leaseToken, expectedStage, nextStage, providerOperationID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) RetryLightsailJob(ctx context.Context, jobID, leaseToken, expectedStage, errorCode string, delay time.Duration) error {
	if !upgradeIDPattern.MatchString(jobID) || !upgradeIDPattern.MatchString(leaseToken) || expectedStage == "" || len(errorCode) < 1 || len(errorCode) > 120 || delay < time.Second || delay > 30*time.Minute {
		return ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `UPDATE lightsail_jobs SET state='retrying',error_code=$4,next_attempt_at=clock_timestamp()+$5::interval,lease_token=NULL,lease_expires_at=NULL,updated_at=clock_timestamp()
 WHERE id=$1 AND state='leased' AND lease_token=$2 AND lease_expires_at>clock_timestamp() AND stage=$3`, jobID, leaseToken, expectedStage, errorCode, delay.String())
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) BeginLightsailExecution(ctx context.Context, jobID, leaseToken string) (LightsailExecution, error) {
	if !upgradeIDPattern.MatchString(jobID) || !upgradeIDPattern.MatchString(leaseToken) {
		return LightsailExecution{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return LightsailExecution{}, err
	}
	defer tx.Rollback(ctx)
	var instanceID, script string
	err = tx.QueryRow(ctx, `SELECT j.instance_id::text,i.script_body FROM lightsail_jobs j JOIN lightsail_instances i ON i.id=j.instance_id
 WHERE j.id=$1 AND j.state='leased' AND j.lease_token=$2 AND j.lease_expires_at>clock_timestamp() AND j.kind='create' AND j.stage='execute-script'
 AND i.execution_started_at IS NULL FOR UPDATE OF j,i`, jobID, leaseToken).Scan(&instanceID, &script)
	if errors.Is(err, pgx.ErrNoRows) {
		return LightsailExecution{}, ErrConflict
	}
	if err != nil {
		return LightsailExecution{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE lightsail_instances SET execution_started_at=clock_timestamp(),script_body=NULL,deployment_state='running',updated_at=clock_timestamp() WHERE id=$1`, instanceID); err != nil {
		return LightsailExecution{}, err
	}
	instance, err := scanLightsailInstance(tx.QueryRow(ctx, `SELECT `+lightsailInstanceColumns+` FROM lightsail_instances WHERE id=$1`, instanceID))
	if err != nil {
		return LightsailExecution{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return LightsailExecution{}, err
	}
	return LightsailExecution{Instance: instance, Script: script}, nil
}

func (s *Store) CompleteLightsailExecution(ctx context.Context, jobID, leaseToken string, result LightsailExecutionResult) error {
	if !upgradeIDPattern.MatchString(jobID) || !upgradeIDPattern.MatchString(leaseToken) || (result.State != "succeeded" && result.State != "failed" && result.State != "unknown") || len(result.Output) > 32768 || len(result.ErrorCode) > 120 || len(result.HostKeySHA256) > 16 {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var instanceID string
	err = tx.QueryRow(ctx, `UPDATE lightsail_jobs SET state=$3,error_code=$4,lease_token=NULL,lease_expires_at=NULL,completed_at=clock_timestamp(),updated_at=clock_timestamp()
 WHERE id=$1 AND state='leased' AND lease_token=$2 AND lease_expires_at>clock_timestamp() AND kind='create' AND stage='execute-script' RETURNING instance_id::text`, jobID, leaseToken, result.State, result.ErrorCode).Scan(&instanceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE lightsail_instances SET deployment_state=$2,execution_output=$3,exit_code=$4,host_key_sha256=$5,error_code=$6,lifecycle_state='running',revision=revision+1,updated_at=clock_timestamp() WHERE id=$1 AND execution_started_at IS NOT NULL`, instanceID, result.State, result.Output, result.ExitCode, result.HostKeySHA256, result.ErrorCode); err != nil {
		return err
	}
	if err = platformLog(ctx, tx, nil, "lightsail.script-terminal", instanceID, map[string]any{"jobId": jobID, "state": result.State, "exitCode": result.ExitCode, "errorCode": result.ErrorCode}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) CompleteLightsailJob(ctx context.Context, jobID, leaseToken, state, errorCode string) error {
	if !upgradeIDPattern.MatchString(jobID) || !upgradeIDPattern.MatchString(leaseToken) || (state != "succeeded" && state != "failed" && state != "unknown" && state != "manual-review") || len(errorCode) > 120 {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var instanceID, kind string
	err = tx.QueryRow(ctx, `UPDATE lightsail_jobs SET state=$3,error_code=$4,lease_token=NULL,lease_expires_at=NULL,completed_at=clock_timestamp(),updated_at=clock_timestamp()
 WHERE id=$1 AND state='leased' AND lease_token=$2 AND lease_expires_at>clock_timestamp() RETURNING instance_id::text,kind`, jobID, leaseToken, state, errorCode).Scan(&instanceID, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if state == "succeeded" && kind == "stop" {
		_, err = tx.Exec(ctx, `UPDATE lightsail_instances SET aws_state='stopped',lifecycle_state='stopped',revision=revision+1,updated_at=clock_timestamp() WHERE id=$1`, instanceID)
	} else if state == "succeeded" && kind == "destroy" {
		_, err = tx.Exec(ctx, `UPDATE lightsail_instances SET aws_state='',lifecycle_state='destroyed',static_ip_address=NULL,destroyed_at=clock_timestamp(),revision=revision+1,updated_at=clock_timestamp() WHERE id=$1`, instanceID)
	} else if state == "succeeded" && kind == "create" {
		var updatedID string
		err = tx.QueryRow(ctx, `UPDATE lightsail_instances SET lifecycle_state='running',revision=revision+1,updated_at=clock_timestamp()
 WHERE id=$1 AND script_size=0 AND deployment_state='not-requested' AND execution_started_at IS NULL RETURNING id::text`, instanceID).Scan(&updatedID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
	} else if state != "succeeded" {
		_, err = tx.Exec(ctx, `UPDATE lightsail_instances SET lifecycle_state=CASE WHEN $2='manual-review' THEN 'manual-review' ELSE 'error' END,error_code=$3,revision=revision+1,updated_at=clock_timestamp() WHERE id=$1`, instanceID, state, errorCode)
	}
	if err != nil {
		return err
	}
	if err = platformLog(ctx, tx, nil, "lightsail.lifecycle-terminal", instanceID, map[string]any{"jobId": jobID, "kind": kind, "state": state, "errorCode": errorCode}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
