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

package msgconv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"slices"
	"strings"

	"github.com/rs/zerolog"
	"github.com/slack-go/slack"
	"go.mau.fi/util/ffmpeg"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"

	"go.mau.fi/mautrix-slack/pkg/msgconv/matrixfmt"
	"go.mau.fi/mautrix-slack/pkg/slackid"
)

var (
	ErrUnknownMsgType       = errors.New("unknown msgtype")
	ErrMediaDownloadFailed  = errors.New("failed to download media")
	ErrMediaUploadFailed    = errors.New("failed to reupload media")
	ErrMediaConvertFailed   = errors.New("failed to re-encode media")
	ErrMediaOnlyEditCaption = errors.New("only media message caption can be edited")
)

func isMediaMsgtype(msgType event.MessageType) bool {
	return msgType == event.MsgImage || msgType == event.MsgAudio || msgType == event.MsgVideo || msgType == event.MsgFile
}

type ConvertedSlackMessage struct {
	SendReq    slack.MsgOption
	FileUpload *slack.UploadFileParameters
	FileShare  *slack.ShareFileParams

	// textFallback is the message of SendReq with the tables as code blocks, to resend it
	// as split text if Slack rejects SendReq (see FallbackParts).
	textFallback *slack.RichTextBlock
	// fallbackOptions are the options of SendReq other than the thread, edit and broadcast.
	fallbackOptions []slack.MsgOption
	// editTargetTS is the message that an edit updates with the first part.
	editTargetTS string
	// threadTS is the thread of the parts: the thread of the message or of the edited
	// message, or the edited message itself. When empty, the first part starts the thread.
	threadTS string
	// broadcast also sends the reply to the channel; only the first part of the split text.
	broadcast bool
}

// FallbackParts is the message as text in parts of at most matrixfmt.MaxSplitChars
// characters, marked "(1/n)", "(2/n)"…, to send in order with FallbackSendReq if Slack
// rejects SendReq. It's empty for messages without text.
func (conv *ConvertedSlackMessage) FallbackParts() []string {
	if conv.textFallback == nil {
		return nil
	}
	return matrixfmt.SplitText(conv.textFallback)
}

// FallbackSendReq is the request of part i of FallbackParts. firstTS is the timestamp of
// the first part, sent before: the other parts go in its thread when the message wasn't in
// one. An edit updates the edited message with the first part and replies to it with the
// rest, in its thread if it has one.
func (conv *ConvertedSlackMessage) FallbackSendReq(part string, i int, firstTS string) slack.MsgOption {
	opts := []slack.MsgOption{slack.MsgOptionText(part, false)}
	switch {
	case i == 0 && conv.editTargetTS != "":
		// Clear the blocks of the edited message, or Slack keeps showing them.
		opts = append(opts, slack.MsgOptionUpdate(conv.editTargetTS), slack.MsgOptionBlocks())
	case conv.threadTS != "":
		opts = append(opts, slack.MsgOptionTS(conv.threadTS))
	case i > 0:
		opts = append(opts, slack.MsgOptionTS(firstTS))
	}
	if i == 0 && conv.broadcast {
		opts = append(opts, slack.MsgOptionBroadcast())
	}
	return slack.MsgOptionCompose(append(opts, conv.fallbackOptions...)...)
}

func isTableBlock(block slack.Block) bool {
	return block.BlockType() == slack.MBTTable
}

func (mc *MessageConverter) ToSlack(
	ctx context.Context,
	client *slack.Client,
	portal *bridgev2.Portal,
	content *event.MessageEventContent,
	evt *event.Event,
	threadRoot *database.Message,
	editTarget *database.Message,
	origSender *bridgev2.OrigSender,
	isRealUser bool,
) (conv *ConvertedSlackMessage, err error) {
	log := zerolog.Ctx(ctx)

	if evt.Type == event.EventSticker {
		// Slack doesn't have stickers, just bridge stickers as images
		content.MsgType = event.MsgImage
	}

	var editTargetID, editThreadID, threadRootID string
	if editTarget != nil {
		if isMediaMsgtype(content.MsgType) {
			content.MsgType = event.MsgText
			if content.FileName == "" || content.FileName == content.Body {
				return nil, ErrMediaOnlyEditCaption
			}
		}
		var ok bool
		_, _, editTargetID, ok = slackid.ParseMessageID(editTarget.ID)
		if !ok {
			return nil, fmt.Errorf("failed to parse edit target ID")
		}
		editThreadID = editTargetID
		if editTarget.ThreadRoot != "" {
			if _, _, root, ok := slackid.ParseMessageID(editTarget.ThreadRoot); ok {
				editThreadID = root
			}
		}
	}
	if threadRoot != nil {
		threadRootMessageID := threadRoot.ID
		if threadRoot.ThreadRoot != "" {
			threadRootMessageID = threadRoot.ThreadRoot
		}
		var ok bool
		_, _, threadRootID, ok = slackid.ParseMessageID(threadRootMessageID)
		if !ok {
			return nil, fmt.Errorf("failed to parse thread root ID")
		}
	}

	switch content.MsgType {
	case event.MsgText, event.MsgEmote, event.MsgNotice:
		options := make([]slack.MsgOption, 0, 4)
		var blocks []slack.Block
		var fallback *slack.RichTextBlock
		if content.Format == event.FormatHTML {
			blocks = mc.MatrixHTMLParser.ParseBlocks(ctx, content.FormattedBody, content.Mentions, portal)
			if slices.ContainsFunc(blocks, isTableBlock) {
				// The same message with the tables as code blocks, in case Slack rejects it.
				fallback = mc.MatrixHTMLParser.Parse(ctx, content.FormattedBody, content.Mentions, portal)
			} else if len(blocks) == 1 {
				fallback, _ = blocks[0].(*slack.RichTextBlock)
			}
		} else {
			fallback = mc.MatrixHTMLParser.ParseText(ctx, content.Body, content.Mentions, portal)
			blocks = []slack.Block{fallback}
		}
		var threadOption []slack.MsgOption
		if editTargetID != "" {
			threadOption = []slack.MsgOption{slack.MsgOptionUpdate(editTargetID)}
		} else if threadRootID != "" {
			threadOption = []slack.MsgOption{slack.MsgOptionTS(threadRootID)}
		}
		if content.MsgType == event.MsgEmote {
			options = append(options, slack.MsgOptionMeMessage())
		}
		if content.BeeperLinkPreviews != nil && len(content.BeeperLinkPreviews) == 0 {
			options = append(options, slack.MsgOptionDisableLinkUnfurl(), slack.MsgOptionDisableMediaUnfurl())
		}
		if origSender != nil {
			options = append(options, slack.MsgOptionUsername(origSender.FormattedName))
			urlProvider, ok := mc.Bridge.Matrix.(bridgev2.MatrixConnectorWithPublicMedia)
			if ok && origSender.AvatarURL != "" {
				publicAvatarURL := urlProvider.GetPublicMediaAddress(origSender.AvatarURL)
				if publicAvatarURL != "" {
					options = append(options, slack.MsgOptionIconURL(publicAvatarURL))
				}
			}
		}
		sendOptions := append(threadOption, options...)
		if content.BeeperBroadcastOutsideThread {
			sendOptions = append(sendOptions, slack.MsgOptionBroadcast())
		}
		converted := &ConvertedSlackMessage{
			SendReq:         slack.MsgOptionCompose(append([]slack.MsgOption{slack.MsgOptionBlocks(blocks...)}, sendOptions...)...),
			textFallback:    fallback,
			fallbackOptions: options,
			editTargetTS:    editTargetID,
			threadTS:        threadRootID,
			broadcast:       content.BeeperBroadcastOutsideThread,
		}
		if editTargetID != "" {
			converted.threadTS = editThreadID
		}
		return converted, nil
	case event.MsgAudio, event.MsgFile, event.MsgImage, event.MsgVideo:
		data, err := mc.Bridge.Bot.DownloadMedia(ctx, content.URL, content.File)
		if err != nil {
			log.Err(err).Msg("Failed to download Matrix attachment")
			return nil, ErrMediaDownloadFailed
		}

		var filename, caption, captionHTML, subtype string
		if content.FileName == "" || content.FileName == content.Body {
			filename = content.Body
		} else {
			filename = content.FileName
			caption = content.Body
			captionHTML = content.FormattedBody
		}
		if content.MSC3245Voice != nil && content.Info.MimeType != "audio/webm; codecs=opus" && ffmpeg.Supported() {
			data, err = ffmpeg.ConvertBytes(ctx, data, ".webm", []string{}, []string{"-c:a", "copy"}, content.Info.MimeType)
			if err != nil {
				log.Err(err).Msg("Failed to convert voice message")
				return nil, ErrMediaConvertFailed
			}
			// Slack web will upload a webm/opus audio file with a .m4a extension.
			// Slack servers appear to then convert it to m4a with aac.
			filename += ".m4a"
			content.Info.MimeType = "audio/webm; codecs=opus"
			subtype = "slack_audio"
		} else if content.MSC3245Voice != nil && (content.Info.MimeType == "audio/webm; codecs=opus" || content.Info.MimeType == "audio/mp4") {
			subtype = "slack_audio"
			if !strings.HasSuffix(filename, ".m4a") {
				filename += ".m4a"
			}
		}
		_, channelID := slackid.ParsePortalID(portal.ID)
		if !isRealUser {
			fileUpload := &slack.UploadFileParameters{
				Filename:        filename,
				Reader:          bytes.NewReader(data),
				FileSize:        len(data),
				Channel:         channelID,
				ThreadTimestamp: threadRootID,
			}
			// TODO support broadcast outside thread flag
			if caption != "" {
				fileUpload.InitialComment = caption
			}
			return &ConvertedSlackMessage{FileUpload: fileUpload}, nil
		} else {
			resp, err := client.GetFileUploadURL(ctx, slack.GetFileUploadURLParameters{
				Filename: filename,
				Length:   len(data),
				SubType:  subtype,
			})
			if err != nil {
				log.Err(err).Msg("Failed to get file upload URL")
				return nil, ErrMediaUploadFailed
			}
			err = client.UploadToURLB(ctx, resp, content.Info.MimeType, data)
			if err != nil {
				log.Err(err).Msg("Failed to upload file")
				return nil, ErrMediaUploadFailed
			}
			err = client.CompleteFileUpload(ctx, resp)
			if err != nil {
				log.Err(err).Msg("Failed to complete file upload")
				return nil, ErrMediaUploadFailed
			}
			var block slack.Block
			if captionHTML != "" {
				block = mc.MatrixHTMLParser.Parse(ctx, content.FormattedBody, content.Mentions, portal)
			} else if caption != "" {
				block = slack.NewRichTextBlock("", slack.NewRichTextSection(slack.NewRichTextSectionTextElement(caption, nil)))
			}
			fileShare := &slack.ShareFileParams{
				Files:     []string{resp.File},
				Channel:   channelID,
				ThreadTS:  threadRootID,
				Broadcast: content.BeeperBroadcastOutsideThread,
			}
			if block != nil {
				fileShare.Blocks = []slack.Block{block}
			}
			return &ConvertedSlackMessage{FileShare: fileShare}, nil
		}
	default:
		return nil, ErrUnknownMsgType
	}
}

func (mc *MessageConverter) uploadMedia(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, data []byte, content *event.MessageEventContent) error {
	content.Info.Size = len(data)
	if content.Info.Width == 0 && content.Info.Height == 0 && strings.HasPrefix(content.Info.MimeType, "image/") {
		cfg, _, _ := image.DecodeConfig(bytes.NewReader(data))
		content.Info.Width, content.Info.Height = cfg.Width, cfg.Height
	}

	mxc, file, err := intent.UploadMedia(ctx, portal.MXID, data, "", content.Info.MimeType)
	if err != nil {
		return err
	}
	if file != nil {
		content.File = file
	} else {
		content.URL = mxc
	}
	return nil
}
