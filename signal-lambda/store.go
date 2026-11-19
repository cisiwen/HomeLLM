// DynamoDB access. Three tables (see deploy/cdk/lib/homellm-stack.ts):
//   - devices: one row per home server - its secret hash, name, and current connection.
//   - access:  one row per (device, user) pairing; UserIndex lists a user's devices.
//   - state:   everything short-lived, keyed by a prefixed "pk" and reaped by TTL (expiresAt):
//     conn#<connectionId>, code#<sha256(pairing code)>, req#<approvalId>, sess#<sessionId>.
//     $disconnect isn't guaranteed to fire, and nothing else here cleans up, so every state
//     row must carry an expiresAt.
package main

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

var (
	devicesTable = os.Getenv("DEVICES_TABLE")
	accessTable  = os.Getenv("ACCESS_TABLE")
	stateTable   = os.Getenv("STATE_TABLE")
)

const (
	// API Gateway closes every WebSocket after 2 hours, so a connection row older than this
	// is certainly stale.
	connectionTTL = 3 * time.Hour
	sessionTTL    = time.Hour
	approvalTTL   = 100 * time.Second
	maxCodeTTL    = 15 * time.Minute
)

type device struct {
	DeviceID     string `dynamodbav:"deviceId"`
	SecretHash   string `dynamodbav:"secretHash"`
	Name         string `dynamodbav:"name"`
	ConnectionID string `dynamodbav:"connectionId,omitempty"`
	Instance     string `dynamodbav:"instance,omitempty"`
	LastSeen     int64  `dynamodbav:"lastSeen"`
}

type access struct {
	DeviceID  string `dynamodbav:"deviceId"`
	UserID    string `dynamodbav:"userId"`
	CreatedAt int64  `dynamodbav:"createdAt"`
}

// state is any short-lived row; which fields are set depends on its pk prefix.
type state struct {
	PK           string `dynamodbav:"pk"`
	Role         string `dynamodbav:"role,omitempty"` // conn#: "home" or "phone"
	DeviceID     string `dynamodbav:"deviceId,omitempty"`
	UserID       string `dynamodbav:"userId,omitempty"`
	ConnectionID string `dynamodbav:"connectionId,omitempty"` // req#/sess#: the phone's connection
	ExpiresAt    int64  `dynamodbav:"expiresAt"`
}

func (s *state) expired() bool { return time.Now().Unix() > s.ExpiresAt }

type store struct{ ddb *dynamodb.Client }

func key(name, value string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{name: &types.AttributeValueMemberS{Value: value}}
}

func (s *store) getDevice(ctx context.Context, id string) (*device, error) {
	out, err := s.ddb.GetItem(ctx, &dynamodb.GetItemInput{TableName: &devicesTable, Key: key("deviceId", id), ConsistentRead: aws.Bool(true)})
	if err != nil || out.Item == nil {
		return nil, err
	}
	var d device
	return &d, attributevalue.UnmarshalMap(out.Item, &d)
}

// createDevice registers a new device; it fails if the ID is already taken.
func (s *store) createDevice(ctx context.Context, d *device) error {
	item, err := attributevalue.MarshalMap(d)
	if err != nil {
		return err
	}
	_, err = s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &devicesTable, Item: item,
		ConditionExpression: aws.String("attribute_not_exists(deviceId)"),
	})
	return err
}

func (s *store) setDeviceConnection(ctx context.Context, id, connID, instance, name string) error {
	expr := "SET connectionId = :c, instance = :i, lastSeen = :t"
	values := map[string]types.AttributeValue{
		":c": &types.AttributeValueMemberS{Value: connID},
		":i": &types.AttributeValueMemberS{Value: instance},
		":t": &types.AttributeValueMemberN{Value: itoa(time.Now().Unix())},
	}
	if name != "" {
		expr += ", #n = :n"
		values[":n"] = &types.AttributeValueMemberS{Value: name}
	}
	in := &dynamodb.UpdateItemInput{TableName: &devicesTable, Key: key("deviceId", id), UpdateExpression: &expr, ExpressionAttributeValues: values}
	if name != "" {
		in.ExpressionAttributeNames = map[string]string{"#n": "name"}
	}
	_, err := s.ddb.UpdateItem(ctx, in)
	return err
}

// clearDeviceConnection marks the device offline, but only if connID is still its current
// connection - a late $disconnect from a replaced connection mustn't take the new one offline.
func (s *store) clearDeviceConnection(ctx context.Context, id, connID string) error {
	_, err := s.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: &devicesTable, Key: key("deviceId", id),
		UpdateExpression:          aws.String("REMOVE connectionId, instance"),
		ConditionExpression:       aws.String("connectionId = :c"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":c": &types.AttributeValueMemberS{Value: connID}},
	})
	if isConditionFailed(err) {
		return nil
	}
	return err
}

func (s *store) hasAccess(ctx context.Context, deviceID, userID string) (bool, error) {
	out, err := s.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &accessTable,
		Key: map[string]types.AttributeValue{
			"deviceId": &types.AttributeValueMemberS{Value: deviceID},
			"userId":   &types.AttributeValueMemberS{Value: userID},
		},
	})
	return err == nil && out.Item != nil, err
}

func (s *store) grantAccess(ctx context.Context, deviceID, userID string) error {
	item, err := attributevalue.MarshalMap(access{DeviceID: deviceID, UserID: userID, CreatedAt: time.Now().Unix()})
	if err != nil {
		return err
	}
	_, err = s.ddb.PutItem(ctx, &dynamodb.PutItemInput{TableName: &accessTable, Item: item})
	return err
}

func (s *store) userDevices(ctx context.Context, userID string) ([]string, error) {
	out, err := s.ddb.Query(ctx, &dynamodb.QueryInput{
		TableName: &accessTable, IndexName: aws.String("UserIndex"),
		KeyConditionExpression:    aws.String("userId = :u"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":u": &types.AttributeValueMemberS{Value: userID}},
	})
	if err != nil {
		return nil, err
	}
	var rows []access
	if err := attributevalue.UnmarshalListOfMaps(out.Items, &rows); err != nil {
		return nil, err
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.DeviceID
	}
	return ids, nil
}

func (s *store) putState(ctx context.Context, st *state) error {
	item, err := attributevalue.MarshalMap(st)
	if err != nil {
		return err
	}
	_, err = s.ddb.PutItem(ctx, &dynamodb.PutItemInput{TableName: &stateTable, Item: item})
	return err
}

func (s *store) getState(ctx context.Context, pk string) (*state, error) {
	out, err := s.ddb.GetItem(ctx, &dynamodb.GetItemInput{TableName: &stateTable, Key: key("pk", pk), ConsistentRead: aws.Bool(true)})
	if err != nil || out.Item == nil {
		return nil, err
	}
	var st state
	if err := attributevalue.UnmarshalMap(out.Item, &st); err != nil {
		return nil, err
	}
	if st.expired() { // TTL deletion lags by up to a day or two
		return nil, nil
	}
	return &st, nil
}

// takeState deletes a row and returns what it held - atomically, so a single-use row (a
// pairing code, an approval) can only ever be consumed once.
func (s *store) takeState(ctx context.Context, pk string) (*state, error) {
	out, err := s.ddb.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: &stateTable, Key: key("pk", pk), ReturnValues: types.ReturnValueAllOld})
	if err != nil || out.Attributes == nil {
		return nil, err
	}
	var st state
	if err := attributevalue.UnmarshalMap(out.Attributes, &st); err != nil {
		return nil, err
	}
	if st.expired() {
		return nil, nil
	}
	return &st, nil
}

func (s *store) deleteState(ctx context.Context, pk string) error {
	_, err := s.ddb.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: &stateTable, Key: key("pk", pk)})
	return err
}

// claimSession records that session belongs to this phone connection. It fails with
// errSessionTaken if another connection already owns it.
func (s *store) claimSession(ctx context.Context, sessionID, connID, deviceID, userID string) error {
	item, err := attributevalue.MarshalMap(state{
		PK: "sess#" + sessionID, DeviceID: deviceID, UserID: userID, ConnectionID: connID,
		ExpiresAt: time.Now().Add(sessionTTL).Unix(),
	})
	if err != nil {
		return err
	}
	_, err = s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &stateTable, Item: item,
		ConditionExpression:       aws.String("attribute_not_exists(pk) OR connectionId = :c"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":c": &types.AttributeValueMemberS{Value: connID}},
	})
	if isConditionFailed(err) {
		return errSessionTaken
	}
	return err
}

var errSessionTaken = errors.New("session belongs to another connection")

func isConditionFailed(err error) bool {
	var cf *types.ConditionalCheckFailedException
	return errors.As(err, &cf)
}
