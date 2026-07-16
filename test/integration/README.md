# Integration test

Stands up a throwaway environment (VPC + public subnet, ECS cluster, ASG running the
plugin build under test, an EBS volume and a MongoDB service that mounts it) and drives
the plugin through its failure modes.

Run it from GitHub Actions: **Actions → Integration test → Run workflow**. It builds the
plugin, uploads it to S3, `terraform apply`, runs the scenarios, then always destroys.

## Auth & required config

The workflows authenticate to AWS via **GitHub OIDC** (no long-lived keys) using
dedicated test secrets, kept separate from the release pipeline.

| Repo secret | Purpose |
|-------------|---------|
| `OIDC_TEST_ROLE_ARN` | Role assumed via OIDC in the sandbox account (must allow the resources in `main.tf`) |
| `AWS_TEST_REGION` | Region for the test environment |

Everything else is ephemeral: Terraform creates a throwaway S3 bucket, uploads the built
plugin tarball to it, and destroys it on teardown. No pre-existing bucket needed.

## Workflow inputs

- `deployment_maximum_percent` — `200` (fix active) or `100`.
- `grant_stop_task` — grant `ecs:StopTask` to the instance role. Set **false** to verify
  the fail-safe: without the permission the plugin must never force-detach a live volume.

## Scenarios

Both scenarios assert one hard invariant — the volume is recovered and the data survives.
The escalation path taken (clean detach vs force-detach) is the expectation for the kernel
state; an unexpected path emits a non-fatal `::warning::` with diagnostics instead of failing.

| Scenario | Asserts |
|----------|---------|
| baseline | plugin mounts, sentinel written/read on the volume |
| healthy handover (redeploy) | data survives the volume handover; a force-detach here is unexpected (warns) |
| wedged holder | the holder's kernel is sysrq-panicked (the 2026-07-15 incident); ECS drains it so the plugin walks StopTask → non-force → force-detach; recovers on the other instance with data intact, force-detach expected |

Assertions come from a sentinel file on the EBS volume (data integrity) and the plugin's
docker journal on the host (whether a force-detach happened) — read via SSM Run Command,
which is headless, unlike `ecs execute-command`.

## Local

The environment runs on arm64 (t4g) instances, so the plugin is built arm64 and the
workflow uses a native ARM runner (`ubuntu-24.04-arm`) — no QEMU. The plugin build arch
must always match `instance_type`.

```sh
make -C ../.. tar-arm64                        # build the plugin tarball (arm64)
terraform init
terraform apply -var plugin_tarball_path=../../polarity-ecs-ebs-plugin.arm64.tar.gz
./run-tests.sh
terraform destroy -var plugin_tarball_path=../../polarity-ecs-ebs-plugin.arm64.tar.gz
```
