import * as fs from 'fs';
import { Stack, StackProps, RemovalPolicy, Duration, CfnOutput } from 'aws-cdk-lib';
import * as s3 from 'aws-cdk-lib/aws-s3';
import * as s3deploy from 'aws-cdk-lib/aws-s3-deployment';
import * as cloudfront from 'aws-cdk-lib/aws-cloudfront';
import * as origins from 'aws-cdk-lib/aws-cloudfront-origins';
import * as acm from 'aws-cdk-lib/aws-certificatemanager';
import * as route53 from 'aws-cdk-lib/aws-route53';
import * as targets from 'aws-cdk-lib/aws-route53-targets';
import { Construct } from 'constructs';

export interface HomeLlmWebStackProps extends StackProps {
  // e.g. homellm.eiuoo.com
  domain: string;
  // The Route 53 hosted zone the domain lives in, e.g. eiuoo.com.
  hostedZoneDomain: string;
  // ACM certificate in us-east-1 (CloudFront requires that region) covering `domain`.
  certificateArn: string;
  // The web client: this repo's web/ folder.
  siteDir: string;
  // The dubbing studio's page: video-dubbing's static/ folder, served at /dubbing/app/. Its
  // server runs on the user's PC and is reached through HomeLLM (see web/dubbing/).
  dubbingDir?: string;
  // HomeLLM Studio's page (this repo's cmd/studio/web), served at /studio/app/ the same way.
  studioDir?: string;
}

/**
 * The HomeLLM web client at homellm.eiuoo.com: static files in a private S3 bucket behind
 * CloudFront. It's only the page - signing in goes straight to Cognito, setup goes through
 * HomeLlmStack's signalling WebSocket, and everything else travels directly between the
 * browser and the user's own PC over WebRTC. So this stack holds no data and no secrets.
 */
export class HomeLlmWebStack extends Stack {
  constructor(scope: Construct, id: string, props: HomeLlmWebStackProps) {
    super(scope, id, props);

    if (!fs.existsSync(`${props.siteDir}/index.html`)) {
      throw new Error(`HomeLLM web client not found at ${props.siteDir} (set -c homellmWebDir=<path to HomeLLM/web>)`);
    }

    const bucket = new s3.Bucket(this, 'SiteBucket', {
      blockPublicAccess: s3.BlockPublicAccess.BLOCK_ALL,
      encryption: s3.BucketEncryption.S3_MANAGED,
      enforceSSL: true,
      // Rebuilt from the web folder on every deploy.
      removalPolicy: RemovalPolicy.DESTROY,
      autoDeleteObjects: true,
    });

    // The page talks only to itself, Cognito and the signalling WebSocket; pictures and clips
    // from the PC are shown as blob: URLs. Inline style attributes are allowed (the page sets a
    // few); inline scripts are not. Framing is same-site only: /dubbing/ shows the dubbing
    // studio in a frame.
    const csp = [
      "default-src 'self'",
      "script-src 'self'",
      "style-src 'self' 'unsafe-inline'",
      "img-src 'self' blob: data:",
      "media-src 'self' blob:",
      `connect-src 'self' https://cognito-idp.${this.region}.amazonaws.com wss://*.execute-api.${this.region}.amazonaws.com`,
      "frame-ancestors 'self'",
      "frame-src 'self'",
      "worker-src 'self'",
      "base-uri 'none'",
      "form-action 'self'",
    ].join('; ');
    const headers = new cloudfront.ResponseHeadersPolicy(this, 'SecurityHeaders', {
      securityHeadersBehavior: {
        contentSecurityPolicy: { contentSecurityPolicy: csp, override: true },
        strictTransportSecurity: { accessControlMaxAge: Duration.days(365), includeSubdomains: false, override: true },
        contentTypeOptions: { override: true },
        frameOptions: { frameOption: cloudfront.HeadersFrameOption.SAMEORIGIN, override: true },
        referrerPolicy: { referrerPolicy: cloudfront.HeadersReferrerPolicy.NO_REFERRER, override: true },
      },
    });

    const distribution = new cloudfront.Distribution(this, 'Distribution', {
      domainNames: [props.domain],
      certificate: acm.Certificate.fromCertificateArn(this, 'Certificate', props.certificateArn),
      defaultRootObject: 'index.html',
      minimumProtocolVersion: cloudfront.SecurityPolicyProtocol.TLS_V1_2_2021,
      priceClass: cloudfront.PriceClass.PRICE_CLASS_ALL,
      defaultBehavior: {
        origin: origins.S3BucketOrigin.withOriginAccessControl(bucket),
        viewerProtocolPolicy: cloudfront.ViewerProtocolPolicy.REDIRECT_TO_HTTPS,
        cachePolicy: cloudfront.CachePolicy.CACHING_OPTIMIZED,
        responseHeadersPolicy: headers,
        // S3 has no folder index pages: serve /dubbing/ as /dubbing/index.html and so on.
        functionAssociations: [{
          eventType: cloudfront.FunctionEventType.VIEWER_REQUEST,
          function: new cloudfront.Function(this, 'IndexPages', {
            runtime: cloudfront.FunctionRuntime.JS_2_0,
            code: cloudfront.FunctionCode.fromInline(
              "function handler(event) { var r = event.request; if (r.uri.endsWith('/')) r.uri += 'index.html'; return r; }"),
          }),
        }],
      },
    });

    // Browsers revalidate (cheap, ETag) so a new deploy shows up on the next load.
    const site = new s3deploy.BucketDeployment(this, 'Deploy', {
      sources: [s3deploy.Source.asset(props.siteDir)],
      destinationBucket: bucket,
      distribution,
      distributionPaths: ['/*'],
      cacheControl: [s3deploy.CacheControl.noCache()],
      prune: true,
      // Owned by the deployments below; excluded so pruning here doesn't delete them.
      exclude: ['dubbing/app/*', 'studio/app/*'],
    });
    // The pages of web apps on the user's PC, each shown by HomeLLM's /<app>/ bridge page.
    const apps: [string, string | undefined][] = [['dubbing', props.dubbingDir], ['studio', props.studioDir]];
    for (const [app, dir] of apps) {
      if (!dir) continue;
      if (!fs.existsSync(`${dir}/index.html`)) {
        throw new Error(`${app} page not found at ${dir} (set -c ${app}WebDir=<path>)`);
      }
      const d = new s3deploy.BucketDeployment(this, app === 'dubbing' ? 'DeployDubbing' : `Deploy${app[0].toUpperCase()}${app.slice(1)}`, {
        sources: [s3deploy.Source.asset(dir)],
        destinationBucket: bucket,
        destinationKeyPrefix: `${app}/app/`,
        distribution,
        distributionPaths: [`/${app}/app/*`],
        cacheControl: [s3deploy.CacheControl.noCache()],
        prune: true,
      });
      d.node.addDependency(site);
    }

    const zone = route53.HostedZone.fromLookup(this, 'Zone', { domainName: props.hostedZoneDomain });
    const recordName = props.domain.endsWith('.' + props.hostedZoneDomain)
      ? props.domain.slice(0, -(props.hostedZoneDomain.length + 1))
      : props.domain;
    const target = route53.RecordTarget.fromAlias(new targets.CloudFrontTarget(distribution));
    new route53.ARecord(this, 'AliasA', { zone, recordName, target });
    new route53.AaaaRecord(this, 'AliasAAAA', { zone, recordName, target });

    new CfnOutput(this, 'Url', { value: `https://${props.domain}/` });
    new CfnOutput(this, 'DistributionId', { value: distribution.distributionId });
  }
}
