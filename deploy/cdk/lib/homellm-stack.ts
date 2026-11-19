import * as path from 'path';
import { Stack, StackProps, Duration, RemovalPolicy, CfnOutput } from 'aws-cdk-lib';
import * as apigwv2 from 'aws-cdk-lib/aws-apigatewayv2';
import * as apigwv2Integrations from 'aws-cdk-lib/aws-apigatewayv2-integrations';
import * as dynamodb from 'aws-cdk-lib/aws-dynamodb';
import * as ssm from 'aws-cdk-lib/aws-ssm';
import { GoFunction } from '@aws-cdk/aws-lambda-go-alpha';
import { Construct } from 'constructs';

// HomeLLM's repo root (this file is deploy/cdk/lib/).
const REPO_ROOT = path.join(__dirname, '..', '..', '..');

/**
 * Signalling for HomeLLM - see signal-lambda/main.go. Moved here from StreamerInfraStructure;
 * the stack name and construct IDs are unchanged, so it's the same deployed stack (same URL,
 * same tables). Shares only the Cognito user pool with the streaming app, read from the SSM
 * parameters CognitoInfraStack publishes.
 */
export class HomeLlmStack extends Stack {
  constructor(scope: Construct, id: string, props?: StackProps) {
    super(scope, id, props);

    const cognitoUserPoolId = ssm.StringParameter.valueForStringParameter(this, '/streamer/cognito/userPoolId');
    const cognitoClientId = ssm.StringParameter.valueForStringParameter(this, '/streamer/cognito/clientId');

    // Pairings are the one piece of real user data here (re-pairing every phone is a chore),
    // so unlike the state table these two are retained if the stack is ever deleted.
    const devicesTable = new dynamodb.Table(this, 'HomeLlmDevicesTable', {
      partitionKey: { name: 'deviceId', type: dynamodb.AttributeType.STRING },
      billingMode: dynamodb.BillingMode.PAY_PER_REQUEST,
      removalPolicy: RemovalPolicy.RETAIN,
    });
    const accessTable = new dynamodb.Table(this, 'HomeLlmAccessTable', {
      partitionKey: { name: 'deviceId', type: dynamodb.AttributeType.STRING },
      sortKey: { name: 'userId', type: dynamodb.AttributeType.STRING },
      billingMode: dynamodb.BillingMode.PAY_PER_REQUEST,
      removalPolicy: RemovalPolicy.RETAIN,
    });
    accessTable.addGlobalSecondaryIndex({
      indexName: 'UserIndex',
      partitionKey: { name: 'userId', type: dynamodb.AttributeType.STRING },
      projectionType: dynamodb.ProjectionType.KEYS_ONLY,
    });
    // Connections, pairing codes, pending approvals and sessions - all short-lived, all reaped
    // by TTL (a WebSocket $disconnect isn't guaranteed to fire; see signal-lambda/store.go).
    const stateTable = new dynamodb.Table(this, 'HomeLlmStateTable', {
      partitionKey: { name: 'pk', type: dynamodb.AttributeType.STRING },
      billingMode: dynamodb.BillingMode.PAY_PER_REQUEST,
      removalPolicy: RemovalPolicy.DESTROY,
      timeToLiveAttribute: 'expiresAt',
    });

    const fn = new GoFunction(this, 'HomeLlmSignalingFunction', {
      entry: path.join(REPO_ROOT, 'signal-lambda'),
      timeout: Duration.seconds(10), // signalling relay only
      memorySize: 256,
      environment: {
        DEVICES_TABLE: devicesTable.tableName,
        ACCESS_TABLE: accessTable.tableName,
        STATE_TABLE: stateTable.tableName,
        COGNITO_USER_POOL_ID: cognitoUserPoolId,
        COGNITO_CLIENT_ID: cognitoClientId,
        COGNITO_REGION: this.region,
        // Scripted testing only: empty (disabled) unless set in the deploying shell.
        TEST_BYPASS_SECRET: process.env.HOMELLM_TEST_BYPASS_SECRET ?? '',
      },
    });
    devicesTable.grantReadWriteData(fn);
    accessTable.grantReadWriteData(fn);
    stateTable.grantReadWriteData(fn);

    // One Lambda behind every route: $default dispatches on the message's "action" field
    // itself, so adding an action never needs an API Gateway change.
    const integration = (name: string) => new apigwv2Integrations.WebSocketLambdaIntegration(name, fn);
    const api = new apigwv2.WebSocketApi(this, 'HomeLlmWebSocketApi', {
      connectRouteOptions: { integration: integration('HomeLlmConnectIntegration') },
      disconnectRouteOptions: { integration: integration('HomeLlmDisconnectIntegration') },
      defaultRouteOptions: { integration: integration('HomeLlmDefaultIntegration') },
    });
    const stage = new apigwv2.WebSocketStage(this, 'HomeLlmWebSocketStage', {
      webSocketApi: api,
      stageName: 'prod',
      autoDeploy: true,
    });
    // Caps what a misbehaving or malicious client can cost: a phone sends a handful of
    // messages per connection and a home server one ping a minute.
    (stage.node.defaultChild as apigwv2.CfnStage).defaultRouteSettings = {
      throttlingRateLimit: 20,
      throttlingBurstLimit: 50,
    };
    // PostToConnection (relaying) and DeleteConnection (dropping a replaced home server).
    stage.grantManagementApiAccess(fn);

    new CfnOutput(this, 'HomeLlmWebSocketUrl', { value: stage.url });
  }
}
