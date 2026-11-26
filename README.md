# ecsd

Deploys to Amazon ECS that end with proof the new revision is what serves, and a way back.

```
ecsd deploy --stage prod
```

1. **Supersede check.** It skips the deploy if the commit is already released or behind the released one, so a slow CI run can't put an older commit over a newer one. It refuses a commit the stage was rolled back from, unless `--force`. With `skip_unless_changed`, it also skips when nothing that goes into the image changed. A changed template is never skipped: if the manifest names a template the released revisions weren't copied from, the deploy runs, even for an already-released commit. The exception is a commit the stage was rolled back to: the rollback chose its config, so a rerun is skipped.
2. **Pin and verify images.** Every image must come from the manifest's registry, and is pinned to its digest (ECR tags are resolved), so the release record and any rollback to it mean the same bytes. `verify_image` runs for each one, such as a check of its provenance attestation.
3. **Register.** Each service gets a new revision copied from the template your infrastructure owns, with only the image changed. It never copies the family's latest revision, which may be an older deploy's. A rerun reuses the revisions it already registered.
4. **Gates.** `before_roll` drains running jobs and runs your checks.
5. **Release task.** Migrations run as a one-off task from the new revision, one at a time per app. If another release task is running, whether a deploy's or one started with `ecsd run --release`, ecsd waits for it and never stops it. If two start together, the older one runs. A cancelled deploy stops its own task. The task's logs are printed and its exit code decides whether the deploy continues.
6. **Roll.** Every service rolls in parallel, and each rollout is followed by its deployment ID until it completes and the old tasks are gone. A circuit-breaker rollback counts as a failure. `aws ecs wait services-stable` reports it as success, because the service is stable, just on the old revision.
7. **Health.** Each endpoint must report the deployed commit three times in a row, so an old task still behind the load balancer can't pass the check.
8. **Smoke.** Requests that must get the expected status and body.
9. **Record.** Before the roll and again before recording, ecsd re-reads the current release. If a rollback, or a deploy of a commit this one doesn't include, was recorded meanwhile, it stops; if that happened mid-roll, it returns the services to that release. Task families get their new revisions only now, once every check has passed. The release (commit, pinned images, revisions) is written to an SSM parameter whose versions form the history. With `on_failure: rollback`, a failure in steps 6–8 returns the services to the current release.

Outside a deploy, the newest revision of every family is the current release's, so an app that starts tasks by family name runs released code. A deploy that fails at any step puts a copy of the released revision back on top of each family it registered.

`ecsd rollback` returns the stage to a recorded release: copies of its revisions on top of every family, services rolled onto them, and no migrations. The copies carry the release's whole config (roles, secrets, env); `--images-only` puts its images into the current templates instead, for when that config has since gone. `ecsd console` is `heroku run` for ECS.

## How it fits

Your infrastructure code (Terraform, CDK, CloudFormation, Pulumi, or by hand) keeps owning the cluster, the services, the load balancers and the roles. ecsd owns only which revision each service runs. Two files divide the work:

| | Where | Says | Written by |
|---|---|---|---|
| `ecsd.yml` | your repository | **what** to deploy: images, migrations, gates, checks | you, versioned with the code it deploys |
| stage manifest | SSM parameter `/<app>/<stage>/deploy` | **where**: cluster, task definition templates, network, hosts | your infrastructure code |

The infrastructure code needs two changes:
- Register a **template** task definition for each service, using any image.
- Have the service **ignore task definition changes**, e.g. Terraform's `lifecycle { ignore_changes = [task_definition] }`.

From then on, a code deploy needs no Terraform state and no infrastructure permissions. `ecsd iam-policy` prints exactly what it does need.

## `ecsd.yml`

```yaml
# yaml-language-server: $schema=https://rkingon.github.io/ecsd/schema/v1/config.json
app: shop
images:
  "*": ${registry}/shop-${name}:${sha}           # every service and task, by name
  worker: { image: "${registry}/shop-worker:${sha}", container: worker }
  web: { image: "${registry}/shop-web:${sha}", container: [assets, web] }  # containers sharing one image

release:                                         # before the rollout, one at a time per app
  task: api
  command: [./api, migrate]

health:                                          # must report the deployed commit
  - { url: "${hosts.api}/healthz", revision: .revision }

smoke:
  - { url: "${hosts.api}/v1/keys", method: POST, headers: { Authorization: "Bearer ${env.SMOKE_KEY}" }, body: "{}", expect: 401 }
  - { url: "${hosts.www}/", contains: "<title>Shop" }

on_failure: rollback
console: { task: console, command: [bash] }
verify_image: gh attestation verify "oci://$ECSD_IMAGE" --repo you/shop --deny-self-hosted-runners
```

`verify_image` is required: a command that fails for an image you don't trust, or `none` to deploy without checking, which every deploy then warns about.

Optional:

```yaml
skip_unless_changed: ["!infra/**", "!**/*.md"]   # deploy only when the image's inputs changed
before_roll:
  - drain: { started_by: scheduler, timeout: 100m }   # wait for jobs a restart would lose
  - exec: ./scripts/check-flags.sh
image_policy: { any_registry: true, unpinned: true }   # opt out of the image rules
timeouts: { roll: 25m }
```

Variables are `${sha}`, `${app}`, `${stage}`, `${registry}`, `${name}` (in images), `${hosts.<name>}` and `${env.<NAME>}`. A missing variable is an error rather than an empty string. The full reference is the [schema](schema/v1/config.json), and `ecsd check` validates the file offline.

## The stage manifest

```json
{
  "cluster": "shop-prod",
  "registry": "123456789012.dkr.ecr.us-east-1.amazonaws.com",
  "services": {
    "api": { "template": "arn:aws:ecs:us-east-1:123456789012:task-definition/shop-prod-api:14" },
    "www": { "template": "arn:aws:ecs:us-east-1:123456789012:task-definition/shop-prod-www:9" }
  },
  "tasks": {
    "console": { "template": "arn:aws:ecs:us-east-1:123456789012:task-definition/shop-prod-console:3",
                 "network": { "subnets": ["subnet-0a…"], "security_groups": ["sg-0c…"] } }
  },
  "hosts": { "api": "https://api.shop.example", "www": "https://shop.example" }
}
```

- `services` are rolled.
- `tasks` are registered on every deploy without a service. They cover consoles, one-off jobs, and tasks your app starts itself with RunTask, which then pick up the new code too.
- One-off tasks use their own `network`, else the manifest's `network`, else the first service's.

See [docs/manifest.md](docs/manifest.md) for writing it from Terraform, CDK, CloudFormation and Pulumi, and the [schema](schema/v1/manifest.json) for every field. To start from a running cluster, run `ecsd manifest init --cluster shop-prod`.

## GitHub Actions

```yaml
deploy:
  runs-on: ubuntu-latest
  environment: prod
  concurrency: { group: deploy-prod, cancel-in-progress: false }
  permissions: { id-token: write, contents: read }
  steps:
    - uses: actions/checkout@v7
      with: { fetch-depth: 0 }                   # the supersede check needs history
    - uses: aws-actions/configure-aws-credentials@v6
      with: { role-to-assume: "${{ vars.DEPLOY_ROLE }}", aws-region: us-east-1 }
    - uses: rkingon/ecsd@<commit sha>            # vX.Y.Z
      with: { stage: prod }
```

The action downloads the release that matches the commit it's pinned to. Before running it, the action checks the binary's checksum and its [build provenance](https://docs.github.com/actions/security-for-github-actions/using-artifact-attestations): it must have been built from that exact commit, by this repository's release workflow, on GitHub-hosted runners. A moved tag or a replaced release fails this check. Its outputs are `skipped`, `sha` and `version`. To skip a build early, run `ecsd superseded`, which only sets `skipped`. To deploy a digest promoted from another registry, use `args: --image api=…@sha256:…`.

## Commands

| | |
|---|---|
| `deploy` | the pipeline above (`--sha`, `--image name=ref`, `--force`, `--no-rollback`, `--skip-gates`) |
| `plan` | what `deploy` would do |
| `rollback` | to the release before the current one, or `--to <commit>` / `--to v12`; `--images-only` keeps the current templates |
| `superseded` | whether a deploy would do nothing |
| `history` | releases, newest first |
| `status` | each service's revision and rollout, the current release, tasks that failed in the last hour |
| `run <name> -- cmd` | a one-off task from the released revision; exits with its exit code |
| `run --release [-- cmd]` | `release.command` (or `cmd`) from the released revision, taking the release lock a deploy's release task takes |
| `run … --from-template` | either of the above from the manifest's template instead of the released revision: one-off tasks before a stage's first deploy |
| `console` | your own one-off task with a shell in it, stopped when you leave (`--keep`, `--stop`) |
| `exec <service> -- cmd` | a command in a running task |
| `tunnel <service> [host:]port` | a local port forwarded into a task, or through it to a database |
| `logs [name…]` | CloudWatch logs (`--since`, `--filter`, `-f`) |
| `insights <query>` | a Logs Insights query over the stage's log groups |
| `manifest show \| validate \| init` | read, check against the account, or start one |
| `iam-policy [--operator]` | the least-privilege policy for deploys, or for people operating the stage |
| `check` | validate `ecsd.yml` offline |
| `schema config \| manifest` | the JSON Schemas |

`console`, `exec` and `tunnel` need AWS's [session-manager-plugin](https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html), which the AWS CLI needs for the same commands.

## Things to know

- **Migrations must be backward compatible.** The old revision keeps serving while the release task runs, and a rollback leaves the schema migrated.
- **The release history is the parameter's versions.** SSM keeps the latest 100. The parameter uses the Intelligent-Tiering tier, so it is billed as advanced only if a release record outgrows 4 KB.
- **Only rolling deployments with the ECS deployment controller are supported.** Turn the deployment circuit breaker on; otherwise a rollout whose tasks never start runs until `timeouts.roll`. `ecsd manifest validate` warns about it. It also warns when the release task wouldn't stop once its command exits: a release container that isn't essential, such as an init container a server depends on.
- **Two deploys of one stage at once:** use a concurrency group. Without one, ecsd serializes an app's release tasks and catches a newer release before rolling and before recording, but SSM has no conditional write: two deploys that roll at the same moment race, and the last to record wins.
- **A cancelled deploy leaves the services where it stopped.** CI kills a cancelled job seconds later, so ecsd doesn't start a rollback it can't finish. The families still hold the released revisions, so `run` and `console` use released code while the services may run newer; `ecsd status` shows it. The next deploy settles it.
- **Images come from one registry, pinned.** An image outside the manifest's `registry`, or a tag outside ECR without a digest, stops the deploy unless `image_policy` allows it.

## Install

```
go install github.com/rkingon/ecsd/cmd/ecsd@latest
```

Or download a binary from the [releases](https://github.com/rkingon/ecsd/releases) and verify it with `gh attestation verify <file> --repo rkingon/ecsd`.

MIT licensed.
