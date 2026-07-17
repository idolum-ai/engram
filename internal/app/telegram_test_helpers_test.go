package app

func telegramReplyMessageID(body map[string]any) int {
	parameters, _ := body["reply_parameters"].(map[string]any)
	messageID, _ := parameters["message_id"].(float64)
	return int(messageID)
}
