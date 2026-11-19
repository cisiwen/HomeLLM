#!/usr/bin/env node
import * as path from 'path';
import { App } from 'aws-cdk-lib';
import { HomeLlmWebStack } from '../lib/homellm-web-stack';

// The HomeLLM web client at https://homellm.<hostedZoneDomain> (see lib/homellm-web-stack.ts).
// It publishes this repo's web/ and cmd/studio/web/ folders and video-dubbing's static/ folder
// (a sibling checkout), so deploy it from a machine with both repos side by side:
//   cd deploy/web-cdk && npm ci && npx cdk deploy HomeLlmWebStack
// Pinned to the account and region it was created in, so wrong credentials fail instead of
// creating a second copy somewhere else. (It moved here from StreamerInfraStructure/cdk; the
// stack name and everything in it are unchanged, so CloudFormation sees the same stack.)
const app = new App();
const repo = path.join(__dirname, '..', '..', '..');
const ctx = (key: string): string => {
  const v = app.node.tryGetContext(key);
  if (typeof v !== 'string' || !v.trim()) throw new Error(`Missing CDK context "${key}" (cdk.json or -c ${key}=...)`);
  return v.trim();
};
new HomeLlmWebStack(app, 'HomeLlmWebStack', {
  env: { account: '323455446472', region: 'ap-southeast-2' },
  domain: `homellm.${ctx('hostedZoneDomain')}`,
  hostedZoneDomain: ctx('hostedZoneDomain'),
  // ACM certificate in us-east-1 covering homellm.<hostedZoneDomain> (the *.eiuoo.com one).
  certificateArn: ctx('certificateArn'),
  siteDir: app.node.tryGetContext('homellmWebDir') ?? path.join(repo, 'web'),
  studioDir: app.node.tryGetContext('studioWebDir') ?? path.join(repo, 'cmd', 'studio', 'web'),
  dubbingDir: app.node.tryGetContext('dubbingWebDir') ?? path.join(repo, '..', 'video-dubbing', 'static'),
});
