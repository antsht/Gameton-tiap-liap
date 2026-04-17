package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type Client struct {
	BaseURL    string
	AuthToken  string
	HTTPClient *http.Client
}

func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL:    baseURL,
		AuthToken:  token,
		HTTPClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (c *Client) req(method, path string, body interface{}, out interface{}) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal err: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, c.BaseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("new request err: %w", err)
	}
	// Добавляем X-Auth-Token согласно правилам турнира API
	req.Header.Set("X-Auth-Token", c.AuthToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("do %s %s err: %w", method, path, err)
	}
	defer resp.Body.Close()
	
	respBodyBytes, _ := io.ReadAll(resp.Body)

	// Сохраняем сырой ответ от сервера в отдельную папку
	os.MkdirAll("logs/api_raw", 0755)
	safePath := strings.ReplaceAll(path, "/", "_")
	filename := fmt.Sprintf("logs/api_raw/%d_%s%s.json", time.Now().UnixNano(), method, safePath)
	os.WriteFile(filename, respBodyBytes, 0644)

	if resp.StatusCode >= 400 {
		var publicErr PublicError
		if err := json.Unmarshal(respBodyBytes, &publicErr); err == nil && len(publicErr.Errors) > 0 {
			return fmt.Errorf("API error %s %s (code %d): %v", method, path, publicErr.Code, publicErr.Errors)
		}
		return fmt.Errorf("API returned status: %s. Body: %s", resp.Status, string(respBodyBytes))
	}

	if out != nil {
		if err := json.Unmarshal(respBodyBytes, out); err != nil {
			return fmt.Errorf("decode err on %s %s: %w", method, path, err)
		}
	}

	return nil
}

func (c *Client) GetArena() (*PlayerResponse, error) {
	var res PlayerResponse
	if err := c.req(http.MethodGet, "/api/arena", nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *Client) PostCommand(cmd PlayerCommand) error {
	return c.req(http.MethodPost, "/api/command", cmd, nil)
}

func (c *Client) GetLogs() ([]LogMessage, error) {
	var logs []LogMessage
	if err := c.req(http.MethodGet, "/api/logs", nil, &logs); err != nil {
		return nil, err
	}
	return logs, nil
}
