package handlers

import (
	"encoding/json"
	"io"
	"time"

	"github.com/ChatGPT-Hackers/ChatGPT-API-server/types"
	"github.com/ChatGPT-Hackers/ChatGPT-API-server/utils"
	"github.com/gin-gonic/gin"
)

const DEFAULT_RETRY_TIME int = 5

func verifyApiKey(c *gin.Context) bool {
	verified, err := utils.VerifyToken(c.Request.Header["Authorization"][0])
	if err != nil {
		c.SSEvent("error", "Failed to verify API key")
		// c.JSON(500, gin.H{
		// 	"error": "Failed to verify API key",
		// })
		return false
	}
	if !verified {
		c.SSEvent("error", "Invalid API key")
		// c.JSON(401, gin.H{
		// 	"error": "Invalid API key",
		// })
		return false
	}
	return true
}

func checkConversationId(
	request *types.ChatGptRequest,
	connection *types.Connection,
	c *gin.Context,
) (*types.Connection, bool) {
	// Check conversation id
	connectionPool.Mu.RLock()
	defer connectionPool.Mu.RUnlock()
	// Check number of connections
	if len(connectionPool.Connections) == 0 {
		c.JSON(503, gin.H{
			"error": "No available clients",
		})
		return nil, false
	}
	if request.ConversationId == "" {
		// Find connection with the lowest load and where heartbeat is after last message time
		for _, conn := range connectionPool.Connections {
			if connection == nil || conn.LastMessageTime.Before(connection.LastMessageTime) {
				if conn.Heartbeat.After(conn.LastMessageTime) {
					connection = conn
				}
			}
		}
		// Check if connection was found
		if connection == nil {
			c.SSEvent("error", "No available clients")
			// c.JSON(503, gin.H{
			// 	"error": "No available clients",
			// })
			return nil, false
		}
	} else {
		// Check if conversation exists
		conversation, ok := conversationPool.Get(request.ConversationId)
		if !ok {
			// Error
			c.SSEvent("error", "Conversation doesn't exists")
			// c.JSON(500, gin.H{
			// 	"error": "Conversation doesn't exists",
			// })
			return nil, false
		} else {
			// Get connectionId of the conversation
			connectionId := conversation.ConnectionId
			// Check if connection exists
			connection, ok = connectionPool.Get(connectionId)
			if !ok {
				// TODO: Maybe instead of returning an error that the associated connection doesn't exist anymore,
				// we can associate the conversation with a new connection from connectionPool.


				// Error
				c.SSEvent("error", "Connection no longer exists")
				// c.JSON(500, gin.H{
				// 	"error": "Connection no longer exists",
				// })
				return nil, false
			}
		}
	}
	return connection, true
}

func sendAndWaitForResponse(request *types.ChatGptRequest, jsonRequest []byte, c *gin.Context) (types.ChatGptResponse, bool) {
	var connection *types.Connection
	var err error

	connection, isValid := checkConversationId(request, connection, c)
	if !isValid && connection == nil {
		return types.ChatGptResponse{}, false
	}

	// Ping before sending request
	if !ping(connection.Id) {
		c.JSON(503, gin.H{
			"error": "Ping failed",
		})
		return types.ChatGptResponse{}, false
	}

	message := types.Message{
		Id:      utils.GenerateId(),
		Message: "ChatGptRequest",
		// Convert request to json
		Data: string(jsonRequest),
	}
	err = connection.Ws.WriteJSON(message)
	if err != nil {
		c.JSON(500, gin.H{
			"error": "Failed to send request to the client",
		})
		// Delete connection
		connectionPool.Delete(connection.Id)
		
		// TODO: Somehow associate the conversation with a new connection from connectionPool
		// TODO: Retry sending the request to the new connection
		// checkConversationId(request, connection, c) function already checks if the conversation is 
		// associated with a connection.

		// the connection failed,
		return types.ChatGptResponse{}, false
	}
	// Set last message time
	connection.LastMessageTime = time.Now()
	// Wait for response with a timeout
	for {
		// Read message
		var receive types.Message
		connection.Ws.SetReadDeadline(time.Now().Add(120 * time.Second))
		err = connection.Ws.ReadJSON(&receive)
		if err != nil {
			c.JSON(500, gin.H{
				"error": "Failed to read response from the client",
				"err":   err.Error(),
			})

			// TODO: Somehow associate the conversation with a new connection from connectionPool

			// Delete connection
			connectionPool.Delete(connection.Id)
			return types.ChatGptResponse{}, false
		}
		// Check if the message is the response
		if receive.Id == message.Id {
			// Convert response to ChatGptResponse
			var response types.ChatGptResponse
			err = json.Unmarshal([]byte(receive.Data), &response)
			if err != nil {
				c.JSON(500, gin.H{
					"error":    "Failed to convert response to ChatGptResponse",
					"response": receive,
				})
				return types.ChatGptResponse{}, false
			}
			// Add conversation to pool
			conversation := &types.Conversation{
				Id:           response.ConversationId,
				ConnectionId: connection.Id,
			}
			conversationPool.Set(conversation)
			// Send response
			c.JSON(200, response)
			// Heartbeat
			connection.Heartbeat = time.Now()
			return response, true
		} else {
			// Error
			c.JSON(500, gin.H{
				"error": "Failed to find response from the client",
			})
			return types.ChatGptResponse{}, false
		}
	}
}

// // # API routes
func API_ask(c *gin.Context) {
	var returnWithoutRetry bool = false
	c.Header("Content-Type", "text/event-stream")

	// Get request
	var request types.ChatGptRequest
	err := c.BindJSON(&request)
	if err != nil {
		c.SSEvent("error", "Invalid request")
		// c.JSON(400, gin.H{
		// 	"error": "Invalid request",
		// })
		returnWithoutRetry = true
		return
	}
	// Check if "Authorization" in Header
	if c.Request.Header["Authorization"] == nil {
		c.SSEvent("error", "API key not provided")
		// c.JSON(401, gin.H{
		// 	"error": "API key not provided",
		// })
		returnWithoutRetry = true
		return
	}

	// Check if API key is valid
	if !verifyApiKey(c) {
		returnWithoutRetry = true
		return
	}

	// If Id is not set, generate a new one
	if request.MessageId == "" {
		request.MessageId = utils.GenerateId()
	}
	// If parent id is not set, generate a new one
	if request.ParentId == "" {
		request.ParentId = utils.GenerateId()
	}
	jsonRequest, err := json.Marshal(request)
	if err != nil {
		c.SSEvent("error", "Failed to convert request to json")
		// c.JSON(500, gin.H{
		// 	"error": "Failed to convert request to json",
		// })
		returnWithoutRetry = true
		return
	}

	stream := make(chan *types.ChatGptResponse, DEFAULT_RETRY_TIME)
	go func() {
		defer close(stream)
		if returnWithoutRetry {
			return
		}

		// Right now, it doesn't retry with a new connection from connectionPool
		// since each conversation id is associated with a connection id
		// and if sending chtagpt request fails or reading response from connection fails,
		// connection id is deleted from connectionPool and we send error instead of associating 
		// the conversation id with a new connection id from connectionPool.
		for i := 0; i < DEFAULT_RETRY_TIME; i++ {
			response, wasSuccessful := sendAndWaitForResponse(&request, jsonRequest, c)
			if wasSuccessful {
				if i == 4 {
					stream <- &response
					break
				}
			}
			time.Sleep(1 * time.Second)
			stream <- nil
		}
	}()
	c.Stream(func(w io.Writer) bool {
		if returnWithoutRetry {
			return false
		}
		if response, ok := <-stream; ok {
			if response != nil {
				out, _ := json.Marshal(response)
				c.SSEvent("chatgpt-response", string(out))
				return true
			}
			return false
		}
		return false
	})

}

func getConnections() []*types.Connection {
	// Get connections
	var connections []*types.Connection
	connectionPool.Mu.RLock()
	for _, connection := range connectionPool.Connections {
		connections = append(connections, connection)
	}
	connectionPool.Mu.RUnlock()
	return connections
}

func API_getConnections(c *gin.Context) {
	var connections = getConnections()
	// Send connections
	c.JSON(200, gin.H{
		"connections": connections,
	})
}

func ping(connection_id string) bool {
	// Get connection
	connection, ok := connectionPool.Get(connection_id)
	// Send "ping" to the connection
	if ok {
		id := utils.GenerateId()
		send := types.Message{
			Id:      id,
			Message: "ping",
		}
		err := connection.Ws.WriteJSON(send)
		if err != nil {
			// Delete connection
			connectionPool.Delete(connection_id)
			return false
		}
		// Wait for response with a timeout
		for {
			// Read message
			var receive types.Message
			connection.Ws.SetReadDeadline(time.Now().Add(5 * time.Second))
			err = connection.Ws.ReadJSON(&receive)
			if err != nil {
				// Delete connection
				connectionPool.Delete(connection_id)
				return false
			}
			// Check if the message is the response
			if receive.Id == send.Id {
				return true
			} else {
				// Error
				return false
			}
		}
	}
	return false
}
