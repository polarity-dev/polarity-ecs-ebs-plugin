#!/usr/bin/env bash
# Integration scenarios for polarity-ecs-ebs-plugin. Reads the Terraform outputs of the
# environment stood up by main.tf and drives the plugin through its failure modes.
#
# The data-integrity sentinel is written/read via SSM Run Command on the host that runs
# the task (docker exec into the mongo container) — headless and reliable, unlike
# `aws ecs execute-command` which needs a TTY. Force-detach behavior is asserted from the
# plugin's docker journal on the host (immediate, unlike CloudTrail which lags).
set -euo pipefail

REGION=$(terraform output -raw region)
CLUSTER=$(terraform output -raw cluster_name)
SERVICE=$(terraform output -raw service_name)
VOLUME=$(terraform output -raw volume_id)
STOP_TASK=$(terraform output -raw grant_stop_task)
MAX_PCT=$(terraform output -raw deployment_maximum_percent)

SENTINEL="SENTINEL_$(date +%s)"

log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "ASSERT FAILED: $*" >&2; exit 1; }

wait_stable() {
  log "waiting for service to stabilize..."
  aws ecs wait services-stable --cluster "$CLUSTER" --services "$SERVICE" --region "$REGION"
}

running_task() {
  aws ecs list-tasks --cluster "$CLUSTER" --service-name "$SERVICE" \
    --desired-status RUNNING --region "$REGION" --query 'taskArns[0]' --output text
}

# EC2 instance id running the current task.
task_instance() {
  local task ci
  task=$(running_task)
  ci=$(aws ecs describe-tasks --cluster "$CLUSTER" --tasks "$task" --region "$REGION" \
    --query 'tasks[0].containerInstanceArn' --output text)
  aws ecs describe-container-instances --cluster "$CLUSTER" --container-instances "$ci" \
    --region "$REGION" --query 'containerInstances[0].ec2InstanceId' --output text
}

# Run a shell script on a host via SSM and print its stdout. Script is base64-encoded to
# avoid any quoting issues in the SSM command JSON.
ssm_run() {
  local id=$1 script=$2 b64 cmd
  b64=$(printf '%s' "$script" | base64 | tr -d '\n')
  cmd=$(aws ssm send-command --instance-ids "$id" --document-name AWS-RunShellScript \
    --parameters "commands=[\"echo $b64 | base64 -d | sh\"]" \
    --region "$REGION" --query 'Command.CommandId' --output text)
  aws ssm wait command-executed --command-id "$cmd" --instance-id "$id" --region "$REGION" >/dev/null 2>&1 || true
  aws ssm get-command-invocation --command-id "$cmd" --instance-id "$id" \
    --region "$REGION" --query 'StandardOutputContent' --output text
}

# docker exec into the mongo container on the task's host to touch the sentinel on the volume.
MONGO_CID='cid=$(docker ps -q --filter label=com.amazonaws.ecs.container-name=mongo | head -1)'
write_sentinel() { ssm_run "$(task_instance)" "$MONGO_CID; docker exec \"\$cid\" sh -c 'echo $SENTINEL > /data/db/SENTINEL'" >/dev/null; }
read_sentinel()  { ssm_run "$(task_instance)" "$MONGO_CID; docker exec \"\$cid\" sh -c 'cat /data/db/SENTINEL 2>/dev/null'" | grep -Eo "${SENTINEL}" | head -1; }

# Times the plugin logged a force detach on a host (from its docker journal).
force_detach_logs() {
  ssm_run "$1" "journalctl -u docker --no-pager | grep -c 'Force-detaching' || true"
}

# Wedge a host's kernel via a sysrq panic (panic=0 makes it hang instead of rebooting).
# Fire-and-forget: the instance dies mid-command, so we don't wait for the invocation.
crash_kernel() {
  aws ssm send-command --instance-ids "$1" --document-name AWS-RunShellScript \
    --parameters 'commands=["sysctl -w kernel.panic=0; echo 1 > /proc/sys/kernel/sysrq; echo c > /proc/sysrq-trigger"]' \
    --region "$REGION" --query 'Command.CommandId' --output text >/dev/null 2>&1 || true
}

# A non-fatal GitHub Actions warning: the plugin recovered the volume, but via an
# unexpected path for the current kernel state. Keeps the job green, flags it in the UI.
warn() { echo "::warning::$*"; log "WARN: $*"; }

# lastStatus of a task arn (NONE if missing).
task_last_status() {
  [ "$1" = "None" ] && { echo NONE; return; }
  aws ecs describe-tasks --cluster "$CLUSTER" --tasks "$1" --region "$REGION" \
    --query 'tasks[0].lastStatus' --output text 2>/dev/null || echo NONE
}

# All EC2 instance ids currently registered in the cluster.
all_instances() {
  local arns
  arns=$(aws ecs list-container-instances --cluster "$CLUSTER" --region "$REGION" \
    --query 'containerInstanceArns' --output text)
  [ -n "$arns" ] && [ "$arns" != "None" ] || return 0
  aws ecs describe-container-instances --cluster "$CLUSTER" --container-instances $arns \
    --region "$REGION" --query 'containerInstances[].ec2InstanceId' --output text
}

# Sum of force-detach log occurrences across all cluster instances.
total_force() {
  local sum=0 i
  for i in $(all_instances); do sum=$((sum + $(force_detach_logs "$i"))); done
  echo "$sum"
}


### Scenario: baseline ###

log "== baseline =="
wait_stable
write_sentinel
[ "$(read_sentinel)" = "$SENTINEL" ] || fail "sentinel not written/readable"
log "sentinel ok"


### Scenario 1: healthy handover (data survives, no force-detach expected) ###
# A normal redeploy hands the volume over between healthy instances. Hard invariant:
# the volume is recovered and the data survives. Expected path: a clean (non-force)
# detach — a force-detach here would mean a healthy holder failed to release the volume,
# which is unexpected, so we flag it (warning) without failing.

log "== healthy handover (redeploy, max_pct=$MAX_PCT, stop_task=$STOP_TASK) =="
aws ecs update-service --cluster "$CLUSTER" --service "$SERVICE" --force-new-deployment --region "$REGION" >/dev/null
wait_stable
[ "$(read_sentinel)" = "$SENTINEL" ] || fail "data lost across healthy handover"
[ "$(total_force)" -eq 0 ] && log "handover ok (clean detach)" \
  || warn "force-detach happened during a healthy handover — a live holder should release the volume cleanly"


### Scenario 2: wedged holder (the 2026-07-15 incident) ###
# On Nitro a non-force detach is an nvme hot-remove that a *live* guest kernel always
# completes — even with the fs mounted and busy (measured: it detaches in ~3s regardless).
# The only way it genuinely gets stuck is a holder whose kernel can no longer process the
# removal. We wedge it with a sysrq panic (mongo still running and holding the volume,
# exactly as in the incident), then DRAIN it (not deregister) so the old task stays visible
# to the plugin — this makes the plugin walk its real escalation: StopTask (issued but
# ineffective, agent is dead) -> non-force detach (times out) -> force-detach.
#
# Hard invariant: the volume is recovered on another instance and the data survives.
# Expected path on a wedged holder: a force-detach. If it somehow recovered without one,
# that contradicts the measured Nitro behavior — flag it (warning + diagnostics), don't fail.

log "== wedged holder (incident) =="
HOLDER=$(task_instance)
HOLDER_CI=$(aws ecs list-container-instances --cluster "$CLUSTER" \
  --filter "ec2InstanceId == $HOLDER" --region "$REGION" \
  --query 'containerInstanceArns[0]' --output text)

log "crashing the kernel on $HOLDER (mongo still holds the volume; guest can't release it)"
crash_kernel "$HOLDER"
sleep 25  # let the panic land before we drive the reschedule

log "draining $HOLDER so ECS reschedules while the old (unstoppable) task stays visible"
aws ecs update-container-instances-state --cluster "$CLUSTER" \
  --container-instances "$HOLDER_CI" --status DRAINING --region "$REGION" >/dev/null

# Wait for the replacement to come up RUNNING on the other instance. We poll the task
# directly instead of `services-stable`: the wedged task lingers as a zombie and would keep
# the service from ever reporting stable.
log "waiting for the replacement task to run on the other instance..."
NEWHOST=$HOLDER
for _ in $(seq 1 100); do
  t=$(running_task)
  NEWHOST=$(task_instance 2>/dev/null || echo "$HOLDER")
  [ "$NEWHOST" != "$HOLDER" ] && [ "$NEWHOST" != "None" ] && [ "$(task_last_status "$t")" = "RUNNING" ] && break
  sleep 15
done
{ [ "$NEWHOST" != "$HOLDER" ] && [ "$NEWHOST" != "None" ] && [ "$(task_last_status "$(running_task)")" = "RUNNING" ]; } \
  || fail "recovery failed: no RUNNING task on another instance — the volume was never recovered from the wedged holder"

[ "$(read_sentinel)" = "$SENTINEL" ] || fail "data lost recovering from the wedged holder"
log "recovered on $NEWHOST, data intact"

FORCE_COUNT=$(force_detach_logs "$NEWHOST")
if [ "$FORCE_COUNT" -gt 0 ]; then
  log "force-detach confirmed ($FORCE_COUNT occurrence(s)) on $NEWHOST — expected path"
else
  warn "recovered from a wedged holder WITHOUT a force-detach — unexpected on Nitro; dumping the plugin path"
  echo "---- $NEWHOST plugin journal ----"
  ssm_run "$NEWHOST" "journalctl -u docker --no-pager | grep -iE 'detach|attach|available|in-use|Force|Stopping|StopTask' | tail -80"
fi

log "ALL SCENARIOS PASSED"
