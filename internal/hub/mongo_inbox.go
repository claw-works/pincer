package hub

import (
	"context"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	mongoOpts "go.mongodb.org/mongo-driver/v2/mongo/options"
)

const inboxTTL = 24 * time.Hour
const inboxCollection = "inbox"

// MongoInbox implements inboxBackend using MongoDB.
type MongoInbox struct {
	coll *mongo.Collection
}

func NewMongoInbox(db *mongo.Database) *MongoInbox {
	coll := db.Collection(inboxCollection)
	// TTL index
	coll.Indexes().CreateOne(context.Background(), mongo.IndexModel{
		Keys:    bson.M{"expires_at": 1},
		Options: mongoOpts.Index().SetExpireAfterSeconds(0),
	})
	return &MongoInbox{coll: coll}
}

type inboxDoc struct {
	ID             string      `bson:"_id"`
	ToAgentID      string      `bson:"to_agent_id"`
	FromAgentID    string      `bson:"from_agent_id"`
	ConversationID string      `bson:"conversation_id"`
	Depth          int         `bson:"depth"`
	Type           string      `bson:"type"`
	Payload        interface{} `bson:"payload"`
	CreatedAt      time.Time   `bson:"created_at"`
	ExpiresAt      time.Time   `bson:"expires_at"`
	Delivered      bool        `bson:"delivered"`
}

func (m *MongoInbox) SaveOffline(agentID string, msg Message) {
	doc := inboxDoc{
		ID:             msg.ID,
		ToAgentID:      agentID,
		FromAgentID:    msg.From,
		ConversationID: msg.ConversationID,
		Depth:          msg.Depth,
		Type:           string(msg.Type),
		Payload:        msg.Payload,
		CreatedAt:      time.Now(),
		ExpiresAt:      time.Now().Add(inboxTTL),
		Delivered:      false,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.coll.InsertOne(ctx, doc); err != nil {
		log.Printf("inbox: save offline failed: %v", err)
	}
}

// InboxMessage is a read-only view of a stored inbox message (for monitor/history).
type InboxMessage struct {
	ID          string      `json:"id"`
	ToAgentID   string      `json:"to_agent_id"`
	FromAgentID string      `json:"from_agent_id"`
	Type        string      `json:"type"`
	Payload     interface{} `json:"payload"`
	CreatedAt   time.Time   `json:"created_at"`
	Delivered   bool        `json:"delivered"`
}

// ListMessages returns inbox history for an agent without marking as delivered.
// Optionally filter by from_agent_id. Results are ordered by created_at desc.
func (m *MongoInbox) ListMessages(agentID string, fromAgentID string, limit int) []InboxMessage {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	filter := bson.M{
		"to_agent_id": agentID,
		"expires_at":  bson.M{"$gt": time.Now()},
	}
	if fromAgentID != "" {
		filter["from_agent_id"] = fromAgentID
	}
	if limit <= 0 {
		limit = 50
	}
	opts := mongoOpts.Find().
		SetSort(bson.M{"created_at": -1}).
		SetLimit(int64(limit))

	cur, err := m.coll.Find(ctx, filter, opts)
	if err != nil {
		return nil
	}
	defer cur.Close(ctx)

	var docs []inboxDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil
	}

	msgs := make([]InboxMessage, len(docs))
	for i, d := range docs {
		msgs[i] = InboxMessage{
			ID:          d.ID,
			ToAgentID:   d.ToAgentID,
			FromAgentID: d.FromAgentID,
			Type:        d.Type,
			Payload:     d.Payload,
			CreatedAt:   d.CreatedAt,
			Delivered:   d.Delivered,
		}
	}
	return msgs
}

// ListConversation returns all messages between two agents (both directions),
// sorted by created_at ascending — suitable for chat view.
func (m *MongoInbox) ListConversation(agentA, agentB string, limit int) []InboxMessage {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if limit <= 0 {
		limit = 100
	}
	filter := bson.M{
		"expires_at": bson.M{"$gt": time.Now()},
		"$or": bson.A{
			bson.M{"to_agent_id": agentA, "from_agent_id": agentB},
			bson.M{"to_agent_id": agentB, "from_agent_id": agentA},
		},
	}
	opts := mongoOpts.Find().
		SetSort(bson.M{"created_at": 1}).
		SetLimit(int64(limit))

	cur, err := m.coll.Find(ctx, filter, opts)
	if err != nil {
		return nil
	}
	defer cur.Close(ctx)

	var docs []inboxDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil
	}

	msgs := make([]InboxMessage, len(docs))
	for i, d := range docs {
		msgs[i] = InboxMessage{
			ID:          d.ID,
			ToAgentID:   d.ToAgentID,
			FromAgentID: d.FromAgentID,
			Type:        d.Type,
			Payload:     d.Payload,
			CreatedAt:   d.CreatedAt,
			Delivered:   d.Delivered,
		}
	}
	return msgs
}

func (m *MongoInbox) PopOffline(agentID string) []Message {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	filter := bson.M{
		"to_agent_id": agentID,
		"delivered":   false,
		"expires_at":  bson.M{"$gt": time.Now()},
	}
	opts := mongoOpts.Find().SetSort(bson.M{"created_at": 1})
	cur, err := m.coll.Find(ctx, filter, opts)
	if err != nil {
		return nil
	}
	defer cur.Close(ctx)

	var docs []inboxDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil
	}
	if len(docs) == 0 {
		return nil
	}

	ids := make([]string, len(docs))
	msgs := make([]Message, len(docs))
	for i, d := range docs {
		ids[i] = d.ID
		msgs[i] = Message{
			ID:             d.ID,
			Type:           MessageType(d.Type),
			From:           d.FromAgentID,
			To:             agentID,
			ConversationID: d.ConversationID,
			Depth:          d.Depth,
			Payload:        d.Payload,
		}
	}
	m.coll.UpdateMany(ctx,
		bson.M{"_id": bson.M{"$in": ids}},
		bson.M{"$set": bson.M{"delivered": true}},
	)
	return msgs
}
