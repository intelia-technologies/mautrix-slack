// mautrix-slack - A Matrix-Slack puppeting bridge.
// Copyright (C) 2024 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package connector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/slack-go/slack"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"go.mau.fi/mautrix-slack/pkg/connector/slackdb"
	"go.mau.fi/mautrix-slack/pkg/emoji"
	"go.mau.fi/mautrix-slack/pkg/msgconv"
	"go.mau.fi/mautrix-slack/pkg/slackid"
)

var (
	_ bridgev2.EditHandlingNetworkAPI        = (*SlackClient)(nil)
	_ bridgev2.RedactionHandlingNetworkAPI   = (*SlackClient)(nil)
	_ bridgev2.ReactionHandlingNetworkAPI    = (*SlackClient)(nil)
	_ bridgev2.ReadReceiptHandlingNetworkAPI = (*SlackClient)(nil)
	_ bridgev2.TypingHandlingNetworkAPI      = (*SlackClient)(nil)
	_ bridgev2.RoomNameHandlingNetworkAPI    = (*SlackClient)(nil)
	_ bridgev2.RoomTopicHandlingNetworkAPI   = (*SlackClient)(nil)
	_ bridgev2.MembershipHandlingNetworkAPI  = (*SlackClient)(nil)
)

func (s *SlackClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	if s.Client == nil {
		return nil, bridgev2.ErrNotLoggedIn
	}
	_, channelID := slackid.ParsePortalID(msg.Portal.ID)
	if channelID == "" {
		return nil, errors.New("invalid channel ID")
	}
	conv, err := s.Main.MsgConv.ToSlack(ctx, s.Client, msg.Portal, msg.Content, msg.Event, msg.ThreadRoot, nil, msg.OrigSender, s.IsRealUser)
	if err != nil {
		return nil, err
	}
	timestamp, otherParts, err := s.sendToSlack(ctx, channelID, conv, msg)
	s.saveOtherParts(ctx, msg, channelID, otherParts)
	if err != nil {
		return nil, err
	}
	if timestamp == "" {
		return &bridgev2.MatrixMessageResponse{Pending: true}, nil
	}
	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:        slackid.MakeMessageID(s.TeamID, channelID, timestamp),
			SenderID:  slackid.MakeUserID(s.TeamID, s.UserID),
			Timestamp: slackid.ParseSlackTimestamp(timestamp),
		},
		StreamOrder: slackid.ParseSlackTimestamp(timestamp).UnixMicro(),
	}, nil
}

// saveOtherParts saves the parts of a split message that the response doesn't save, so that
// their echoes from Slack are ignored instead of bridged back to Matrix as new messages.
// They have no Matrix event of their own, so they get a deterministic event ID.
func (s *SlackClient) saveOtherParts(ctx context.Context, msg *bridgev2.MatrixMessage, channelID string, timestamps []string) {
	if len(timestamps) == 0 {
		return
	}
	br := msg.Portal.Bridge
	senderID := slackid.MakeUserID(s.TeamID, s.UserID)
	// The message row needs the ghost row, as bridgev2 ensures before saving a message.
	_, _ = br.GetGhostByID(ctx, senderID)
	for _, timestamp := range timestamps {
		messageID := slackid.MakeMessageID(s.TeamID, channelID, timestamp)
		err := br.DB.Message.Insert(ctx, &database.Message{
			ID:        messageID,
			MXID:      br.Matrix.GenerateDeterministicEventID(msg.Portal.MXID, msg.Portal.PortalKey, messageID, ""),
			Room:      msg.Portal.PortalKey,
			SenderID:  senderID,
			Timestamp: slackid.ParseSlackTimestamp(timestamp),
		})
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Str("slack_ts", timestamp).Msg("Failed to save a part of a split message")
		}
	}
}

func newEmptyChan() chan struct{} {
	return make(chan struct{})
}

func (s *SlackClient) waitForFileCreated(ctx context.Context, fileID string) {
	waitChan := s.fileCreatedListeners.GetOrSetFactory(fileID, newEmptyChan)
	defer s.fileCreatedListeners.Delete(fileID)
	if waitChan != nil {
		select {
		case <-time.After(5 * time.Second):
			zerolog.Ctx(ctx).Warn().Str("file_id", fileID).Msg("Timed out waiting for file_created event from Slack")
		case <-waitChan:
			zerolog.Ctx(ctx).Debug().Str("file_id", fileID).Msg("File create listener fired, sending file message")
		}
	} else {
		zerolog.Ctx(ctx).Debug().Str("file_id", fileID).Msg("File create listener is nil, assuming file_created event was already received")
	}
}

// sendToSlack returns the timestamp of the message. When it was resent as split text, it's
// the timestamp of the first part, and otherParts has those of the other parts sent; if
// the split text failed, otherParts has all the parts that were sent.
func (s *SlackClient) sendToSlack(
	ctx context.Context,
	channelID string,
	conv *msgconv.ConvertedSlackMessage,
	msg *bridgev2.MatrixMessage,
) (string, []string, error) {
	log := zerolog.Ctx(ctx)
	if conv.SendReq != nil {
		log.Debug().Msg("Sending message to Slack")
		_, timestamp, err := s.Client.PostMessageContext(ctx, channelID, conv.SendReq)
		var rejection slack.SlackErrorResponse
		if err != nil && errors.As(err, &rejection) {
			if reqs := conv.FallbackSendReqs(); len(reqs) > 0 {
				log.Warn().Err(err).Int("parts", len(reqs)).Msg("Slack rejected the message, resending it as split text")
				return s.sendSplitText(ctx, channelID, reqs, err)
			}
		}
		return timestamp, nil, err
	} else if conv.FileUpload != nil {
		log.Debug().Msg("Uploading attachment to Slack")
		file, err := s.Client.UploadFileContext(ctx, *conv.FileUpload)
		if err != nil {
			log.Err(err).Msg("Failed to upload attachment to Slack")
			return "", nil, err
		}
		s.waitForFileCreated(ctx, file.ID)
		var shareInfo slack.ShareFileInfo
		// Slack puts the channel message info after uploading a file in either file.shares.private or file.shares.public
		if info, found := file.Shares.Private[channelID]; found && len(info) > 0 {
			shareInfo = info[0]
		} else if info, found = file.Shares.Public[channelID]; found && len(info) > 0 {
			shareInfo = info[0]
		}
		if shareInfo.Ts != "" {
			return shareInfo.Ts, nil, nil
		}
		if msg != nil {
			msg.AddPendingToSave(nil, networkid.TransactionID(fmt.Sprintf("%s:%s", s.UserID, file.ID)), nil)
		}
		return "", nil, nil
	} else if conv.FileShare != nil {
		for _, fileID := range conv.FileShare.Files {
			s.waitForFileCreated(ctx, fileID)
		}
		log.Debug().Strs("files", conv.FileShare.Files).Msg("Sharing already uploaded attachment to Slack")
		resp, err := s.Client.ShareFile(ctx, *conv.FileShare)
		if err != nil {
			log.Err(err).Msg("Failed to share attachment to Slack")
			return "", nil, err
		}
		return resp.FileMsgTS, nil, nil
	} else {
		return "", nil, errors.New("no message or attachment to send")
	}
}

// sendSplitText sends the parts of a message that Slack rejected, in order, and returns the
// timestamp of the first one and those of the rest. If Slack rejects the text too, the
// message fails for good, and all the parts sent are returned as otherParts.
func (s *SlackClient) sendSplitText(ctx context.Context, channelID string, reqs []slack.MsgOption, rejection error) (string, []string, error) {
	sent := make([]string, 0, len(reqs))
	for i, req := range reqs {
		_, timestamp, err := s.Client.PostMessageContext(ctx, channelID, req)
		if err != nil {
			err = fmt.Errorf("slack rejected the message (%w) and part %d/%d of its split text: %w", rejection, i+1, len(reqs), err)
			zerolog.Ctx(ctx).Err(err).Int("parts_sent", i).Msg("Message failed for good")
			return "", sent, bridgev2.WrapErrorInStatus(err).WithStatus(event.MessageStatusFail).WithIsCertain(true).WithSendNotice(true)
		}
		sent = append(sent, timestamp)
	}
	return sent[0], sent[1:], nil
}

func (s *SlackClient) HandleMatrixEdit(ctx context.Context, msg *bridgev2.MatrixEdit) error {
	if s.Client == nil {
		return bridgev2.ErrNotLoggedIn
	}
	_, channelID := slackid.ParsePortalID(msg.Portal.ID)
	if channelID == "" {
		return errors.New("invalid channel ID")
	}
	conv, err := s.Main.MsgConv.ToSlack(ctx, s.Client, msg.Portal, msg.Content, msg.Event, nil, msg.EditTarget, msg.OrigSender, s.IsRealUser)
	if err != nil {
		return err
	}
	msg.EditTarget.Metadata.(*slackid.MessageMetadata).LastEditTS, _, err = s.sendToSlack(ctx, channelID, conv, nil)
	return err
}

func (s *SlackClient) HandleMatrixMessageRemove(ctx context.Context, msg *bridgev2.MatrixMessageRemove) error {
	if s.Client == nil {
		return bridgev2.ErrNotLoggedIn
	}
	_, channelID, messageID, ok := slackid.ParseMessageID(msg.TargetMessage.ID)
	if !ok {
		return errors.New("invalid message ID")
	}
	_, _, err := s.Client.DeleteMessageContext(ctx, channelID, messageID)
	return err
}

func (s *SlackClient) PreHandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (resp bridgev2.MatrixReactionPreResponse, err error) {
	key := msg.Content.RelatesTo.Key
	var emojiID string
	if strings.ContainsRune(key, ':') {
		var dbEmoji *slackdb.Emoji
		dbEmoji, err = s.Main.DB.Emoji.GetByMXC(ctx, key)
		if err != nil {
			err = fmt.Errorf("failed to get emoji from db: %w", err)
		} else if dbEmoji == nil {
			err = fmt.Errorf("unknown emoji %q", key)
		} else {
			emojiID = dbEmoji.EmojiID
		}
	} else {
		emojiID = emoji.GetShortcode(key)
		if emojiID == "" {
			err = fmt.Errorf("unknown emoji %q", key)
		}
	}
	if err != nil {
		return
	}
	return bridgev2.MatrixReactionPreResponse{
		SenderID: slackid.MakeUserID(s.TeamID, s.UserID),
		EmojiID:  networkid.EmojiID(emojiID),
	}, nil
}

func (s *SlackClient) HandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (reaction *database.Reaction, err error) {
	if s.Client == nil {
		return nil, bridgev2.ErrNotLoggedIn
	}
	_, channelID, messageID, ok := slackid.ParseMessageID(msg.TargetMessage.ID)
	if !ok {
		return nil, errors.New("invalid message ID")
	}
	err = s.Client.AddReactionContext(ctx, string(msg.PreHandleResp.EmojiID), slack.ItemRef{
		Channel:   channelID,
		Timestamp: messageID,
	})
	return
}

func (s *SlackClient) HandleMatrixReactionRemove(ctx context.Context, msg *bridgev2.MatrixReactionRemove) error {
	if s.Client == nil {
		return bridgev2.ErrNotLoggedIn
	}
	_, channelID, messageID, ok := slackid.ParseMessageID(msg.TargetReaction.MessageID)
	if !ok {
		return errors.New("invalid message ID")
	}
	err := s.Client.RemoveReactionContext(ctx, string(msg.TargetReaction.EmojiID), slack.ItemRef{
		Channel:   channelID,
		Timestamp: messageID,
	})
	if err != nil && err.Error() != "reaction" {
		return err
	}
	return nil
}

func (s *SlackClient) HandleMatrixReadReceipt(ctx context.Context, msg *bridgev2.MatrixReadReceipt) error {
	if s.Client == nil {
		return bridgev2.ErrNotLoggedIn
	} else if !s.IsRealUser {
		return nil
	}
	if msg.ExactMessage != nil {
		_, channelID, messageTS, ok := slackid.ParseMessageID(msg.ExactMessage.ID)
		if !ok {
			return errors.New("invalid message ID")
		}
		return s.Client.MarkConversationContext(ctx, channelID, messageTS)
	}
	lastMessage, err := s.UserLogin.Bridge.DB.Message.GetLastPartAtOrBeforeTime(ctx, msg.Portal.PortalKey, msg.ReadUpTo)
	if err != nil {
		return err
	} else if lastMessage != nil {
		_, channelID, messageTS, ok := slackid.ParseMessageID(lastMessage.ID)
		if !ok {
			return errors.New("invalid message ID")
		}
		return s.Client.MarkConversationContext(ctx, channelID, messageTS)
	}
	return nil
}

func (s *SlackClient) HandleMatrixTyping(ctx context.Context, msg *bridgev2.MatrixTyping) error {
	if s.Client == nil {
		return bridgev2.ErrNotLoggedIn
	} else if !s.IsRealUser {
		return nil
	}
	_, channelID := slackid.ParsePortalID(msg.Portal.ID)
	if channelID == "" {
		return nil
	}
	s.RTM.SendMessage(s.RTM.NewTypingMessage(channelID))
	return nil
}

func (s *SlackClient) HandleMatrixRoomName(ctx context.Context, msg *bridgev2.MatrixRoomName) (bool, error) {
	_, channelID := slackid.ParsePortalID(msg.Portal.ID)
	if channelID == "" {
		return false, errors.New("invalid channel ID")
	}
	resp, err := s.Client.RenameConversationContext(ctx, channelID, strings.TrimPrefix(msg.Content.Name, "#"))
	zerolog.Ctx(ctx).Trace().Any("resp_data", resp).Msg("Renamed conversation")
	return err == nil, err
}

func (s *SlackClient) HandleMatrixMembership(ctx context.Context, msg *bridgev2.MatrixMembershipChange) (*bridgev2.MatrixMembershipResult, error) {
	if msg.Type.IsSelf && msg.OrigSender != nil {
		return nil, nil
	}

	if s.Client == nil {
		return nil, bridgev2.ErrNotLoggedIn
	}
	_, channelID := slackid.ParsePortalID(msg.Portal.ID)
	if channelID == "" {
		return nil, errors.New("invalid channel ID")
	}
	if msg.Portal.RoomType == database.RoomTypeDM || msg.Portal.RoomType == database.RoomTypeGroupDM {
		return nil, nil
	}

	var targetUserID string
	switch target := msg.Target.(type) {
	case *bridgev2.Ghost:
		_, targetUserID = slackid.ParseUserID(target.ID)
	case *bridgev2.UserLogin:
		_, targetUserID = slackid.ParseUserLoginID(target.ID)
	default:
		return nil, fmt.Errorf("unknown membership target type: %T", target)
	}
	if targetUserID == "" {
		return nil, errors.New("invalid target user ID")
	}

	log := zerolog.Ctx(ctx)
	switch msg.Type {
	case bridgev2.Invite:
		_, err := s.Client.InviteUsersToConversationContext(ctx, channelID, targetUserID)
		if err != nil {
			return nil, fmt.Errorf("failed to invite user to channel: %w", err)
		}
		log.Debug().Str("target_user_id", targetUserID).Msg("Invited user to Slack channel")
	case bridgev2.Leave:
		_, err := s.Client.LeaveConversationContext(ctx, channelID)
		if err != nil {
			return nil, fmt.Errorf("failed to leave channel: %w", err)
		}
		log.Debug().Msg("Left Slack channel")
	case bridgev2.Kick:
		err := s.Client.KickUserFromConversationContext(ctx, channelID, targetUserID)
		if err != nil {
			return nil, fmt.Errorf("failed to kick user from channel: %w", err)
		}
		log.Debug().Str("target_user_id", targetUserID).Msg("Kicked user from Slack channel")
	default:
		return nil, nil
	}
	return nil, nil
}

func (s *SlackClient) HandleMatrixRoomTopic(ctx context.Context, msg *bridgev2.MatrixRoomTopic) (bool, error) {
	_, channelID := slackid.ParsePortalID(msg.Portal.ID)
	if channelID == "" {
		return false, errors.New("invalid channel ID")
	}
	resp, err := s.Client.SetTopicOfConversationContext(ctx, channelID, msg.Content.Topic)
	zerolog.Ctx(ctx).Trace().Any("resp_data", resp).Msg("Changed conversation topic")
	return err == nil, err
}
