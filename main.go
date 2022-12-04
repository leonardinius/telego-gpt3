package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
	gogpt "github.com/sashabaranov/go-gpt3"
)

type contextKey string

// Connect to the SQLite database and create the table to store messages
func init() {
	db, err := sql.Open("sqlite3", "messages.db")
	if err != nil {
		log.Panic(err)
	}
	defer db.Close()

	// Create the table to store messages
	query := `
        CREATE TABLE IF NOT EXISTS messages (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            session_id TEXT,
            user_id TEXT,
            text TEXT,
            created_at timestamp WITH TIMEZONE DEFAULT CURRENT_TIMESTAMP
        );
    `
	_, err = db.Exec(query)
	if err != nil {
		log.Panic(err)
	}

}

// Generate a response using the go-gpt3 package
func generateResponse(ctx context.Context, message *tgbotapi.Message) string {
	bot := ctx.Value(contextKey("bot")).(*tgbotapi.BotAPI)
	client := ctx.Value(contextKey("client")).(*gogpt.Client)

	// Connect to the SQLite database
	db, err := sql.Open("sqlite3", "messages.db")
	if err != nil {
		log.Panic(err)
	}
	defer db.Close()

	sessionID := message.From.ID
	userID := message.From.ID
	botID := bot.Self.ID
	userInput := strings.TrimSpace(message.Text)
	// Store the user's message in the database
	insertQuery := `
        INSERT INTO messages (session_id, user_id, text)
        VALUES (?, ?, ?);
    `
	_, err = db.ExecContext(ctx, insertQuery, sessionID, userID, userInput)
	if err != nil {
		log.Panic(err)
	}

	// Retrieve the previous messages from the database
	const cutOff int = 500
	selectQuery := `
        SELECT
			CASE user_id
				WHEN ? THEN text
				ELSE datetime(created_at, 'localtime') || ' [' || user_id || ']: ' || text
			END
			FROM messages
        WHERE session_id = ?
        ORDER BY created_at DESC
        LIMIT ?;
    `
	rows, err := db.QueryContext(ctx, selectQuery, botID, sessionID, cutOff)
	if err != nil {
		log.Panic(err)
	}
	defer rows.Close()

	// Concatenate the previous messages into a single string
	counter := 0
	var prompt string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			log.Panic(err)
		}
		if text == "" {
			continue
		}
		prompt = text + "\n\n" + prompt
		counter++
	}
	if counter >= cutOff {
		prompt = "... All, but last " + strconv.Itoa(cutOff) + " messages removed.\n\n" + prompt
	}

	// Generate a response using the go-gpt3 package
	response, err := client.CreateCompletion(ctx, gogpt.CompletionRequest{
		Model:       "text-davinci-003",
		Prompt:      prompt,
		MaxTokens:   500,
		Temperature: 0.7,
	})
	if err != nil {
		log.Printf("[ERROR] Err: %#v", err)
	}

	responseText := strings.TrimSpace(responseChoiceText(response))
	_, err = db.ExecContext(ctx, insertQuery, sessionID, botID, responseText)
	if err != nil {
		log.Panic(err)
	}

	return responseText
}

func responseChoiceText(response gogpt.CompletionResponse) string {
	text := ""
	for _, choice := range response.Choices {
		text += choice.Text + "\n"
	}
	return text
}

func main() {
	// Load the environment variables from the .env file
	err := godotenv.Load()
	if err != nil {
		log.Panic(err)
	}

	// Create the OpenAI client
	ctx := context.Background()
	client := gogpt.NewClient(os.Getenv("OPENAI_SECRET_KEY"))

	// Create a new Telegram bot
	bot, err := tgbotapi.NewBotAPI(os.Getenv("TELEGRAM_BOT_TOKEN"))
	if err != nil {
		log.Panic(err)
	}
	// Set the bot's username
	bot.Debug = true
	// Print the bot's username
	fmt.Printf("Authorized on account %s\n", bot.Self.UserName)

	// Get updates from the Telegram API
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	// Create a new context with the bot and client
	ctx = context.WithValue(ctx, contextKey("bot"), bot)
	ctx = context.WithValue(ctx, contextKey("client"), client)

	// Handle each incoming message
	for update := range updates {
		if update.Message == nil {
			continue
		}

		go func() {
			callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			// Generate a response using the go-gpt3 package
			responseText := generateResponse(callCtx, update.Message)
			if responseText == "" {
				responseText = "Err, I don't know what to say."
			}

			if callCtx.Err() != nil {
				return
			}

			responseMessage := tgbotapi.NewMessage(update.Message.Chat.ID, responseText)
			responseMessage.ReplyToMessageID = update.Message.MessageID
			_, err := bot.Send(responseMessage)
			if err != nil {
				log.Printf("[ERROR] Err: %#v", err)
			}
		}()
	}
}
