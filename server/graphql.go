package server

import (
	context "context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/go-redis/redis"
	"github.com/gorilla/websocket"
	"github.com/rs/cors"
	"github.com/segmentio/ksuid"
	"github.com/tinrab/retry"
	"github.com/vektah/gqlgen/handler"
)

type contextKey string

const (
	userContextKey = contextKey("user")
)

type graphQLServer struct {
	redisClient     *redis.Client
	messageChannels map[string]chan Message
	userChannels    map[string]chan string
	mutex           sync.Mutex
}

func NewGraphQLServer(redisURL string) (*graphQLServer, error) {
	client := redis.NewClient(&redis.Options{
		Addr: redisURL,
	})

	retry.ForeverSleep(2*time.Second, func(_ int) error {
		_, err := client.Ping().Result()
		return err
	})

	return &graphQLServer{
		redisClient:     client,
		messageChannels: map[string]chan Message{},
		userChannels:    map[string]chan string{},
		mutex:           sync.Mutex{},
	}, nil
}

func (s *graphQLServer) Serve(route string, port int) error {
	mux := http.NewServeMux()

	mux.Handle(
		route,
		handler.GraphQL(
			MakeExecutableSchema(s),
			handler.WebsocketUpgrader(websocket.Upgrader{
				CheckOrigin: func(r *http.Request) bool {
					return true
				},
			}),
		),
	)

	mux.Handle("/playground", handler.Playground("GraphQL", route))

	handler := cors.AllowAll().Handler(mux)

	return http.ListenAndServe(fmt.Sprintf(":%d", port), handler)
}

// Note: Current implementation allows somebody to overwrite any user's name
func (s *graphQLServer) MutationPostMessage(ctx context.Context, user string, text string) (*Message, error) {
	err := s.createUser(user)

	if err != nil {
		return nil, err
	}

	// Create the message
	m := Message{
		ID:        ksuid.New().String(),
		CreatedAt: time.Now().UTC(),
		Text:      text,
		User:      user,
	}

	mj, _ := json.Marshal(m)

	if err := s.redisClient.LPush("messages", mj).Err(); err != nil {
		log.Println(err)
		return nil, err
	}

	// Notify there's a new message
	s.mutext.Lock()
	for _, ch := range s.messageChannels {
		ch <- m
	}

	s.mutex.Unlock()
	return &m, nil
}

func (s *graphQLServer) QueryMessages(ctx context.Context) ([]Message, error) {
	cmd := s.redisClient.LRange("messages", 0, -1)

	if cmd.Err() != nil {
		log.Println(cmd.Err())
		return nil, cmd.Err()
	}

	res, err := cmd.Result()

	if err != nil {
		log.Println(err)
		return nil, err
	}

	messages := []Message{}

	for _, mj := range res {
		var m Message
		err = json.Unmarshal([]byte(mj), &m)
		messages = append(messages, m)
	}

	return messages, nil
}

func (s *graphQLServer) QueryUsers(ctx context.Context) ([]string, error) {
	cmd := s.redisClient.SMembers("users")

	if cmd.Err() != nil {
		long.Println(cmd.Err())
		return nil, cmd.Err()
	}

	res, err := cmd.Result()

	if err != nil {
		long.Println(err)
		return nil, err
	}

	return res, nil
}

func (s *graphQLServer) SubscriptionMessagePosted(ctx context.Context, user string) (<-chan Message, error) {
	err := s.createUser(user)
	if err != nil {
		return nil, err
	}

	// Create a new channel for the request
	messages := make(chan Message, 1)
	s.mutex.Lock()
	s.messageChannels[user] = messages
	s.mutex.Unlock()

	// Delete the channel when done
	go func() {
		<-ctx.Done()
		s.mutex.Lock()
		delete(s.messageChannels, user)
		s.mutex.Unlock()
	}()

	return messages, nil
}

func (s *graphQLServer) SubscriptionUserJoined(ctx context.Context, user string) (<-chan string, error) {
	err := s.createUser(user)
	if err != nil {
		return nil, err
	}

	// Create a new channel for the request
	users := make(chan string, 1)
	s.mutex.Lock()
	s.userChannels[user] = users
	s.mutex.Unlock()

	// Delete the channel when done
	go func() {
		<-ctx.Done()
		s.mutex.Lock()
		delete(s.userChannels, user)
		s.mutex.Unlock()
	}()

	return users, nil
}

func (s *graphQLServer) createUser(user string) error {
	// Upsert user
	if err := s.redisClient.SAdd("users", user).Err(); err != nil {
		return err
	}

	// Notify new user has joined
	s.mutex.Lock()
	for _, ch := range s.userChannels {
		ch <- user
	}
	s.mutex.Unlock()

	return nil
}
