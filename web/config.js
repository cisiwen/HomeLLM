// Public settings for the HomeLLM web client: the Cognito user pool and app client it signs in
// with (the same ones as the Android app) and the signalling WebSocket. None of these are
// secrets. They come from the CognitoInfraStack and HomeLlmStack outputs; see HomeLLM's README.
window.HOMELLM_CONFIG = {
  region: 'ap-southeast-2',
  userPoolId: 'ap-southeast-2_z1WRw9nUz',
  clientId: '27ubaplgpe7r5gli1fmnp737kf',
  signalUrl: 'wss://it8qx5mjlk.execute-api.ap-southeast-2.amazonaws.com/prod',
};
