package bot

import (
	"github.com/innogames/slack-bot/bot/util"
	"github.com/innogames/slack-bot/client"
	"github.com/innogames/slack-bot/config"
	"github.com/nlopes/slack"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"os"
	"strings"
	"time"
)

// TypeInternal is only used internally to identify internal slack messages.
// @deprecated do not use it anymore
const TypeInternal = "internal"

// NewBot created main bot struct which holds the slack connection and dispatch messages to commands
func NewBot(cfg config.Config, slackClient *client.Slack, logger *log.Logger, commands *Commands) Bot {
	return Bot{
		config:       cfg,
		slackClient:  slackClient,
		logger:       logger,
		commands:     commands,
		allowedUsers: map[string]string{},
	}
}

type Bot struct {
	config       config.Config
	slackClient  *client.Slack
	logger       *log.Logger
	auth         *slack.AuthTestResponse
	commands     *Commands
	allowedUsers map[string]string
}

// Init establish slack connection and load allowed users
func (b *Bot) Init() (err error) {
	if b.config.Slack.Token == "" {
		return errors.Errorf("No slack.token provided in config!")
	}

	b.logger.Infof("Connecting to slack...")
	b.auth, err = b.slackClient.AuthTest()
	if err != nil {
		return errors.Wrap(err, "auth error")
	}

	go b.slackClient.ManageConnection()

	channels, err := b.slackClient.GetChannels(true)
	if err != nil {
		return errors.Wrap(err, "error while fetching public channels")
	}
	client.Channels = make(map[string]string, len(channels))
	for _, channel := range channels {
		client.Channels[channel.ID] = channel.Name
	}

	err = b.loadSlackData()
	if err != nil {
		return err
	}

	if len(b.config.Slack.AutoJoinChannels) > 0 {
		for _, channel := range b.config.Slack.AutoJoinChannels {
			_, err := b.slackClient.JoinChannel(channel)
			if err != nil {
				return err
			}
		}

		b.logger.Infof("Auto joined channels: %s", strings.Join(b.config.Slack.AutoJoinChannels, ", "))
	}

	b.logger.Infof("Loaded %d allowed users and %d channels", len(b.allowedUsers), len(client.Channels))
	b.logger.Infof("Bot user: %s with ID: %s", b.auth.User, b.auth.UserID)
	b.logger.Infof("Initialized %d commands", b.commands.Count())

	return nil
}

// load the public channels and list of all users from current space
func (b *Bot) loadSlackData() error {
	// whitelist users by group
	for _, groupName := range b.config.Slack.AllowedGroups {
		group, err := b.slackClient.GetUserGroupMembers(groupName)
		if err != nil {
			return errors.Wrap(err, "error fetching user of group")
		}
		b.config.AllowedUsers = append(b.config.AllowedUsers, group...)
	}

	// load user list
	allUsers, err := b.slackClient.GetUsers()
	if err != nil {
		return errors.Wrap(err, "error fetching users")
	}
	for _, user := range allUsers {
		// deprecated: whitelist by title
		if b.config.Slack.Team != "" && strings.Contains(user.Profile.Title, b.config.Slack.Team) {
			b.allowedUsers[user.ID] = user.Name
			continue
		}

		for _, allowedUserName := range b.config.AllowedUsers {
			if allowedUserName == user.Name || allowedUserName == user.ID {
				b.allowedUsers[user.ID] = user.Name
				break
			}
		}
	}

	client.Users = b.allowedUsers

	return nil
}

func (b *Bot) Disconnect() error {
	return b.slackClient.Disconnect()
}

// HandleMessages is blocking method to handle new incoming events
func (b *Bot) HandleMessages(kill chan os.Signal) {
	for {
		select {
		case msg := <-b.slackClient.IncomingEvents:
			// message received from user
			switch message := msg.Data.(type) {
			case *slack.MessageEvent:
				if b.shouldHandleMessage(message) {
					go b.HandleMessage(*message)
				}
			case *slack.LatencyReport:
				b.logger.Debugf("Current latency: %v\n", message.Value)
			}
		case msg := <-client.InternalMessages:
			// e.g. triggered by "delay" or "macro" command. They are still executed in original event context
			// -> will post in same channel as the user posted the original command
			msg.SubType = TypeInternal
			b.HandleMessage(msg)
		case <-kill:
			b.Disconnect()
			b.logger.Warnf("Shutdown!")
			return
		}
	}
}

func (b Bot) shouldHandleMessage(event *slack.MessageEvent) bool {
	// exclude all bot traffic
	if event.BotID != "" || event.User == "" || event.User == b.auth.UserID || event.SubType == "bot_message" {
		return false
	}

	// <@Bot> was mentioned in a public channel
	if strings.Contains(event.Text, "<@"+b.auth.UserID+">") {
		return true
	}

	// Direct message channels always starts with 'D'
	if event.Channel[0] == 'D' {
		return true
	}

	return false
}

// remove @bot prefix of message and cleanup
func (b Bot) trimMessage(msg string) string {
	msg = strings.Replace(msg, "<@"+b.auth.UserID+">", "", 1)
	msg = strings.Replace(msg, "‘", "'", -1)
	msg = strings.Replace(msg, "’", "'", -1)

	return strings.TrimSpace(msg)
}

// HandleMessage process the incoming message and respond appropriately
func (b Bot) HandleMessage(event slack.MessageEvent) {
	event.Text = b.trimMessage(event.Text)
	if event.Text == "" {
		return
	}

	start := time.Now()
	logger := b.getLogger(event)

	// send "bot is typing" command
	b.slackClient.RTM.SendMessage(b.slackClient.NewTypingMessage(event.Channel))

	_, existing := b.allowedUsers[event.User]
	if !existing && event.SubType != TypeInternal && b.config.Slack.TestEndpointUrl == "" {
		logger.Errorf("user %s is not allowed to execute message: %s", event.User, event.Text)
		b.slackClient.Reply(event, "Sorry, you are not whitelisted yet. Please ask the slack-bot admin to get access.")
		return
	}

	if !b.commands.Run(event) {
		logger.Infof("Unknown command: %s", event.Text)
		b.sendFallbackMessage(event)
	}

	logger.Infof("handled message: %s in %s", event.Text, util.FormatDuration(time.Now().Sub(start)))
}