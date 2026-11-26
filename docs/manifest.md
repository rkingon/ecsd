# Writing the stage manifest

The manifest is JSON in an SSM parameter, `/<app>/<stage>/deploy` by default. Whatever builds the stage writes it. The
only contract is the [schema](../schema/v1/manifest.json); `ecsd manifest validate` checks it, and ecsd.yml, against
the account.

Each service needs two things from your infrastructure code:

1. A **template** task definition: the real one, with any image. ecsd copies it on every deploy, changing only the
   image, so a change to the template (memory, environment, secrets) ships with the next deploy.
2. A service that **ignores task definition changes**, so applying your infrastructure doesn't undo a deploy.

## Terraform

```hcl
resource "aws_ecs_task_definition" "api" {
  family                = "shop-prod-api"
  container_definitions = jsonencode([{ name = "api", image = "placeholder", essential = true, /* … */ }])
  # …
}

resource "aws_ecs_service" "api" {
  name            = "api"
  task_definition = aws_ecs_task_definition.api.arn
  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }
  lifecycle {
    ignore_changes = [task_definition]
  }
  # …
}

resource "aws_ssm_parameter" "deploy" {
  name = "/shop/prod/deploy"
  type = "String"
  value = jsonencode({
    cluster  = aws_ecs_cluster.this.name
    registry = split("/", aws_ecr_repository.api.repository_url)[0]
    services = { api = { template = aws_ecs_task_definition.api.arn } }
    tasks    = { console = { template = aws_ecs_task_definition.console.arn } }
    network  = { subnets = var.private_subnet_ids, security_groups = [aws_security_group.tasks.id] }
    hosts    = { api = "https://${aws_route53_record.api.fqdn}" }
  })
}
```

Terraform's task definition resource deregisters the revision it replaces. That's harmless: ecsd's revisions are its
own. But the template it registers in its place is the family's newest revision until the next deploy, and an app that
starts tasks by family name runs it, placeholder image and all. To keep templates out of the family apps start, register
them in a family of their own and name the one ecsd registers into:

```hcl
tasks = { worker = { template = aws_ecs_task_definition.worker_template.arn, family = "shop-prod-worker" } }
```

## CDK and CloudFormation

CloudFormation can't ignore a property. When a stack update changes a service's task definition (because you changed
the template), it moves the service onto the template itself, image and all, until the next deploy. So:

- give the template a real, runnable image, not a placeholder;
- run `ecsd deploy` right after any stack update that changes a template. It deploys even an already-released commit when a template changed.

```ts
const api = new ecs.FargateTaskDefinition(this, "Api", { family: "shop-prod-api", cpu: 512, memoryLimitMiB: 1024 });
api.addContainer("api", { image: ecs.ContainerImage.fromEcrRepository(repo, "stable"), /* … */ });

new ecs.FargateService(this, "ApiService", {
  cluster, taskDefinition: api, serviceName: "api", circuitBreaker: { rollback: true },
});

new ssm.StringParameter(this, "Deploy", {
  parameterName: "/shop/prod/deploy",
  stringValue: Stack.of(this).toJsonString({
    cluster: cluster.clusterName,
    registry: `${this.account}.dkr.ecr.${this.region}.amazonaws.com`,
    services: { api: { template: api.taskDefinitionArn } },
    hosts: { api: "https://api.shop.example" },
  }),
});
```

To type it, generate the types from the schema: `npx json-schema-to-typescript schema/v1/manifest.json`.

## CloudFormation

```yaml
DeployManifest:
  Type: AWS::SSM::Parameter
  Properties:
    Name: /shop/prod/deploy
    Type: String
    Value: !Sub |
      {"cluster": "${Cluster}",
       "services": {"api": {"template": "${ApiTaskDefinition}"}},
       "hosts": {"api": "https://api.shop.example"}}
```

## Pulumi

```ts
new aws.ssm.Parameter("deploy", {
  name: "/shop/prod/deploy",
  type: "String",
  value: pulumi.jsonStringify({
    cluster: cluster.name,
    services: { api: { template: apiTaskDefinition.arn } },
    hosts: { api: "https://api.shop.example" },
  }),
});
// The service: ignoreChanges: ["taskDefinition"] in its resource options.
```

## No infrastructure code

`ecsd manifest init --cluster shop-prod` prints a manifest whose templates are what each service runs now. Write it with
`aws ssm put-parameter --name /shop/prod/deploy --type String --value file://manifest.json`.
