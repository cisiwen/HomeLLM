#!/usr/bin/env node
import * as fs from 'fs';
import * as path from 'path';
import { App, Environment } from 'aws-cdk-lib';
import { HomeLlmStack } from '../lib/homellm-stack';
import { HomeLlmWebStack } from '../lib/homellm-web-stack';

// HomeLLM's AWS side, two independent stacks, each deployed on its own:
//   npx cdk deploy HomeLlmStack      signalling (WebSocket API + Lambda + DynamoDB)
//   npx cdk deploy HomeLlmWebStack   https://homellm.<hostedZoneDomain> (S3 + CloudFront)
// Both moved here from StreamerInfraStructure; their names and contents are unchanged, so
// CloudFormation sees the same stacks. They only share the Cognito user pool with the
// streaming app (CognitoInfraStack, deployed from StreamerInfraStructure).
//
// Pinned to the account and region they were created in, so wrong credentials fail instead of
// creating second copies somewhere else.
const env: Environment = { account: '323455446472', region: 'ap-southeast-2' };
const app = new App();
const repo = path.join(__dirname, '..', '..', '..');
const ctx = (key: string): string => {
  const v = app.node.tryGetContext(key);
  if (typeof v !== 'string' || !v.trim()) throw new Error(`Missing CDK context "${key}" (cdk.json or -c ${key}=...)`);
  return v.trim();
};

new HomeLlmStack(app, 'HomeLlmStack', { env });

// The web stack also publishes video-dubbing's page, from a checkout next to this repo. Without
// it the stack is left out of this app (so HomeLlmStack still deploys anywhere), rather than
// deploying a site whose /dubbing/ page would be deleted.
const dubbingDir = app.node.tryGetContext('dubbingWebDir') ?? path.join(repo, '..', 'video-dubbing', 'static');
if (fs.existsSync(path.join(dubbingDir, 'index.html'))) {
  new HomeLlmWebStack(app, 'HomeLlmWebStack', {
    env,
    domain: `homellm.${ctx('hostedZoneDomain')}`,
    hostedZoneDomain: ctx('hostedZoneDomain'),
    // ACM certificate in us-east-1 covering homellm.<hostedZoneDomain> (the *.eiuoo.com one).
    certificateArn: ctx('certificateArn'),
    siteDir: app.node.tryGetContext('homellmWebDir') ?? path.join(repo, 'web'),
    studioDir: app.node.tryGetContext('studioWebDir') ?? path.join(repo, 'cmd', 'studio', 'web'),
    dubbingDir,
  });
} else {
  console.warn(`HomeLlmWebStack left out: no video-dubbing page at ${dubbingDir} (check out video-dubbing next to HomeLLM, or -c dubbingWebDir=...)`);
}
