// Package room implements the public group chat room feature (Issue #25).
// Each User automatically has a default room keyed as "user:{user_id}:default".
// Agents can post messages and list recent messages.
// Messages are stored in MongoDB with a 7-day TTL.
package room

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	mongoOpts "go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	collection  = "room_messages"
	messageTTL  = 7 * 24 * time.Hour // 7 days
	defaultLimit = 20
)

// QuotedMessage is an embedded snapshot of a quoted message.
type QuotedMessage struct {
	ID            string `bson:"id"             json:"id"`
	SenderAgentID string `bson:"sender_agent_id" json:"sender_agent_id"`
	Content       string `bson:"content"        json:"content"`
}

// Message is a single chat room message.
type Message struct {
	ID            string                 `bson:"_id"      json:"id"`
	RoomID        string                 `bson:"room_id"  json:"room_id"`
	SenderAgentID string                 `bson:"sender_agent_id" json:"sender_agent_id"`
	Content       string                 `bson:"content"  json:"content"`
	QuoteID       string                 `bson:"quote_id,omitempty"  json:"quote_id,omitempty"`
	Quote         *QuotedMessage         `bson:"quote,omitempty"     json:"quote,omitempty"`
	Metadata      map[string]interface{} `bson:"metadata,omitempty" json:"metadata,omitempty"`
	CreatedAt     time.Time              `bson:"created_at" json:"created_at"`
	ExpiresAt     time.Time              `bson:"expires_at" json:"-"`
}

// Store handles room message persistence in MongoDB.
type Store struct {
	coll *mongo.Collection
}

// NewStore creates a Store and ensures the TTL index exists.
func NewStore(db *mongo.Database) *Store {
	coll := db.Collection(collection)
	// Ensure TTL index on expires_at
	_, err := coll.Indexes().CreateOne(context.Background(), mongo.IndexModel{
		Keys:    bson.D{{Key: "expires_at", Value: 1}},
		Options: mongoOpts.Index().SetExpireAfterSeconds(0),
	})
	if err != nil {
		log.Printf("room: warn: TTL index: %v", err)
	}
	// Compound index for efficient room queries
	_, err = coll.Indexes().CreateOne(context.Background(), mongo.IndexModel{
		Keys: bson.D{
			{Key: "room_id", Value: 1},
			{Key: "created_at", Value: -1},
		},
	})
	if err != nil {
		log.Printf("room: warn: compound index: %v", err)
	}
	return &Store{coll: coll}
}

// DefaultRoomID returns the default room id for a user.
func DefaultRoomID(userID string) string {
	return fmt.Sprintf("user:%s:default", userID)
}

// Post stores a new message in the room.
// If quoteID is non-empty, the quoted message is looked up and embedded as a snapshot.
func (s *Store) Post(ctx context.Context, roomID, senderAgentID, content, quoteID string, metadata map[string]interface{}) (*Message, error) {
	now := time.Now().UTC()
	msg := &Message{
		ID:            uuid.New().String(),
		RoomID:        roomID,
		SenderAgentID: senderAgentID,
		Content:       content,
		Metadata:      metadata,
		CreatedAt:     now,
		ExpiresAt:     now.Add(messageTTL),
	}
	if quoteID != "" {
		fetchCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		var quoted Message
		if err := s.coll.FindOne(fetchCtx, bson.M{"_id": quoteID}).Decode(&quoted); err == nil {
			msg.QuoteID = quoteID
			// Truncate snapshot content to 200 chars to keep documents small.
			snap := quoted.Content
			if len(snap) > 200 {
				snap = snap[:200] + "…"
			}
			msg.Quote = &QuotedMessage{
				ID:            quoted.ID,
				SenderAgentID: quoted.SenderAgentID,
				Content:       snap,
			}
		} else {
			return nil, fmt.Errorf("room post: quote_id %q not found", quoteID)
		}
	}
	postCtx, cancel2 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel2()
	if _, err := s.coll.InsertOne(postCtx, msg); err != nil {
		return nil, fmt.Errorf("room post: %w", err)
	}
	return msg, nil
}

// GetByID fetches a single message by its ID.
func (s *Store) GetByID(ctx context.Context, id string) (*Message, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var msg Message
	if err := s.coll.FindOne(ctx, bson.M{"_id": id}).Decode(&msg); err != nil {
		return nil, fmt.Errorf("room getbyid: %w", err)
	}
	return &msg, nil
}

// List returns up to limit messages in the room, sorted newest-first.
// If beforeID is set, returns messages older than that message (newest-first).
// If afterID is set, returns messages newer than that message (oldest-first, for polling).
// If since is set (RFC3339/ISO8601), returns messages newer than that time (sorted oldest-first for pagination).
func (s *Store) List(ctx context.Context, roomID string, limit int, beforeID, afterID, since string) ([]*Message, error) {
	if limit <= 0 {
		limit = defaultLimit
	}
	filter := bson.M{"room_id": roomID}
	if beforeID != "" {
		var ref Message
		if err := s.coll.FindOne(ctx, bson.M{"_id": beforeID}).Decode(&ref); err == nil {
			filter["created_at"] = bson.M{"$lt": ref.CreatedAt}
		}
	}
	if afterID != "" {
		var ref Message
		if err := s.coll.FindOne(ctx, bson.M{"_id": afterID}).Decode(&ref); err == nil {
			if existing, ok := filter["created_at"].(bson.M); ok {
				existing["$gt"] = ref.CreatedAt
			} else {
				filter["created_at"] = bson.M{"$gt": ref.CreatedAt}
			}
		}
	}
	if since != "" {
		if sinceTime, err := time.Parse(time.RFC3339Nano, since); err == nil {
			if existing, ok := filter["created_at"].(bson.M); ok {
				existing["$gt"] = sinceTime
			} else {
				filter["created_at"] = bson.M{"$gt": sinceTime}
			}
		}
	}

	// Return oldest-first when paginating forward (after= or since= without before=).
	sortDir := -1
	if (afterID != "" || since != "") && beforeID == "" {
		sortDir = 1
	}
	opts := mongoOpts.Find().
		SetSort(bson.D{{Key: "created_at", Value: sortDir}}).
		SetLimit(int64(limit))

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cur, err := s.coll.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("room list: %w", err)
	}
	defer cur.Close(ctx)

	var msgs []*Message
	if err := cur.All(ctx, &msgs); err != nil {
		return nil, fmt.Errorf("room list decode: %w", err)
	}
	return msgs, nil
}

// Search returns messages matching keyword in content, sorted newest-first.
func (s *Store) Search(ctx context.Context, roomID, keyword string, limit, offset int) ([]*Message, int64, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	filter := bson.M{
		"room_id": roomID,
		"content": bson.M{"$regex": keyword, "$options": "i"},
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	total, err := s.coll.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, err
	}

	opts := mongoOpts.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetLimit(int64(limit)).
		SetSkip(int64(offset))

	cur, err := s.coll.Find(ctx, filter, opts)
	if err != nil {
		return nil, 0, err
	}
	defer cur.Close(ctx)

	var msgs []*Message
	if err := cur.All(ctx, &msgs); err != nil {
		return nil, 0, err
	}
	return msgs, total, nil
}
