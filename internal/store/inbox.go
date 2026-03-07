package store

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	mongoOpts "go.mongodb.org/mongo-driver/v2/mongo/options"
)

// InboxMessage represents a pending message for an offline agent.
type InboxMessage struct {
	ID             string      `bson:"_id" json:"id"`
	ToAgentID      string      `bson:"to_agent_id" json:"to_agent_id"`
	FromAgentID    string      `bson:"from_agent_id" json:"from_agent_id"`
	ConversationID string      `bson:"conversation_id" json:"conversation_id"`
	Depth          int         `bson:"depth" json:"depth"`
	Type           string      `bson:"type" json:"type"`
	Payload        interface{} `bson:"payload" json:"payload"`
	CreatedAt      time.Time   `bson:"created_at" json:"created_at"`
	ExpiresAt      time.Time   `bson:"expires_at" json:"expires_at"`
	Delivered      bool        `bson:"delivered" json:"delivered"`
}

const maxMessageDepth = 10
const inboxTTL = 24 * time.Hour

// SaveInboxMessage stores a message for an offline agent.
func (db *DB) SaveInboxMessage(ctx context.Context, msg InboxMessage) error {
	if msg.Depth > maxMessageDepth {
		return nil // silently drop to prevent loops
	}
	msg.CreatedAt = time.Now()
	msg.ExpiresAt = time.Now().Add(inboxTTL)
	msg.Delivered = false
	_, err := db.Mongo.Collection("inbox").InsertOne(ctx, msg)
	return err
}

// PopInbox retrieves and marks as delivered all pending messages for an agent.
func (db *DB) PopInbox(ctx context.Context, agentID string) ([]InboxMessage, error) {
	filter := bson.M{
		"to_agent_id": agentID,
		"delivered":   false,
		"expires_at":  bson.M{"$gt": time.Now()},
	}
	opts := mongoOpts.Find().SetSort(bson.M{"created_at": 1})
	cur, err := db.Mongo.Collection("inbox").Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var msgs []InboxMessage
	if err := cur.All(ctx, &msgs); err != nil {
		return nil, err
	}

	if len(msgs) > 0 {
		ids := make([]string, len(msgs))
		for i, m := range msgs {
			ids[i] = m.ID
		}
		db.Mongo.Collection("inbox").UpdateMany(ctx,
			bson.M{"_id": bson.M{"$in": ids}},
			bson.M{"$set": bson.M{"delivered": true}},
		)
	}
	return msgs, nil
}

// PeekInbox returns undelivered message count for an agent.
func (db *DB) PeekInbox(ctx context.Context, agentID string) (int64, error) {
	return db.Mongo.Collection("inbox").CountDocuments(ctx, bson.M{
		"to_agent_id": agentID,
		"delivered":   false,
		"expires_at":  bson.M{"$gt": time.Now()},
	})
}
