package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"example.com/m/config"
	"github.com/360EntSecGroup-Skylar/excelize"
	"github.com/go-redis/redis/v8"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ctx = context.Background()

// 初始化 PostgreSQL 和 Redis 連接
var pgConn *pgxpool.Pool
var rdb *redis.Client
var sensitiveWords []string

// Aho-Corasick狀態機結構
type AhoCorasick struct {
	root     *Node
	patterns []string
}

// Node表示Aho-Corasick中的一個節點
type Node struct {
	children map[rune]*Node
	fail     *Node
	output   []string
}

// 新建Aho-Corasick
func NewAhoCorasick() *AhoCorasick {
	return &AhoCorasick{root: &Node{children: make(map[rune]*Node)}}
}

// 插入敏感詞
func (ac *AhoCorasick) Insert(pattern string) {
	node := ac.root
	for _, char := range pattern {
		if _, ok := node.children[char]; !ok {
			node.children[char] = &Node{children: make(map[rune]*Node)}
		}
		node = node.children[char]
	}
	node.output = append(node.output, pattern)
}

// 建立失敗指標
func (ac *AhoCorasick) Build() {
	queue := []*Node{ac.root}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for char, child := range current.children {
			// 設置失敗指標
			failNode := current.fail
			for failNode != nil {
				if next, ok := failNode.children[char]; ok {
					child.fail = next
					break
				}
				failNode = failNode.fail
			}
			if child.fail == nil {
				child.fail = ac.root
			}
			child.output = append(child.output, child.fail.output...)
			queue = append(queue, child)
		}
	}
}

// 用Aho-Corasick過濾消息
func (ac *AhoCorasick) Filter(content string) map[string]int {
	node := ac.root
	results := make(map[string]int)

	for _, char := range content {
		for node != ac.root && node.children[char] == nil {
			node = node.fail
		}
		node = node.children[char]

		if node == nil {
			node = ac.root
		}

		for _, pattern := range node.output {
			results[pattern]++
		}
	}

	return results
}

// 敏感詞初始化函數：從 PostgreSQL 加載敏感詞到 Redis
func loadSensitiveWords() error {
	// 清空 Redis 中舊的敏感詞
	err := rdb.Del(ctx, "sensitive_words").Err()
	if err != nil {
		return err
	}

	// 從 PostgreSQL 中獲取所有敏感詞
	rows, err := pgConn.Query(ctx, "SELECT word FROM sensitive_words")
	if err != nil {
		return err
	}
	defer rows.Close()

	// 將敏感詞加載到 Redis
	for rows.Next() {
		var word string
		if err := rows.Scan(&word); err != nil {
			return err
		}
		sensitiveWords = append(sensitiveWords, word)
		err = rdb.SAdd(ctx, "sensitive_words", word).Err()
		if err != nil {
			return err
		}
	}

	return nil
}

// 更新敏感詞列表並重新加載 Redis
func addSensitiveWord(word string) error {
	// 插入新敏感詞到 PostgreSQL
	_, err := pgConn.Exec(ctx, "INSERT INTO sensitive_words (word) VALUES ($1) ON CONFLICT DO NOTHING", word)
	if err != nil {
		return err
	}

	// 將新詞加載到 Redis
	err = rdb.SAdd(ctx, "sensitive_words", word).Err()
	if err != nil {
		return err
	}

	// 更新敏感詞列表
	sensitiveWords = append(sensitiveWords, word)
	return nil
}

// 從 Excel 文件讀取敏感詞並插入 PostgreSQL
func loadSensitiveWordsFromExcel(filePath string) error {
	// 清空舊的敏感詞
	_, err := pgConn.Exec(ctx, "DELETE FROM sensitive_words")
	if err != nil {
		return err
	}

	// 打開 Excel 文件
	f, err := excelize.OpenFile(filePath)
	if err != nil {
		return err
	}

	// 讀取工作表中的所有行
	rows := f.GetRows("Sheet1") // 根據實際工作表名稱修改

	// 從第二行開始讀取（跳過標題行）
	for i, row := range rows {
		if i == 0 { // 跳過第一行
			continue
		}

		// 遍歷行中的每個詞
		for _, word := range row {
			if word != "" { // 確保詞不為空
				// 插入敏感詞到 PostgreSQL
				_, err := pgConn.Exec(ctx, "INSERT INTO sensitive_words (word) VALUES ($1) ON CONFLICT DO NOTHING", word)
				if err != nil {
					return err
				}
				// 將新詞加載到 Redis
				err = rdb.SAdd(ctx, "sensitive_words", word).Err()
				if err != nil {
					return err
				}
				// 更新敏感詞列表
				sensitiveWords = append(sensitiveWords, word)
			}
		}
	}

	return nil
}

func main() {
	// 初始化資料庫連接、Redis 連接
	var err error
	// 初始化 Redis 客戶端
	rdb, err = config.InitRedis()

	// 初始化 PostgreSQL
	pgConn, err = config.InitDB()

	if err := config.CheckAndCreateTableChat(pgConn); err != nil {
		log.Fatalf("Error checking/creating chat table: %v", err)
	}

	// 從 Excel 文件加載敏感詞
	err = loadSensitiveWordsFromExcel("./combined_sensitive_words.xlsx") // 替換為您的文件路徑
	if err != nil {
		log.Fatalf("Error loading sensitive words from Excel: %v", err)
	}

	// 初始化時加載敏感詞
	err = loadSensitiveWords()
	if err != nil {
		fmt.Println("Error loading sensitive words:", err)
		return
	}

	// 建立 Aho-Corasick 機器並插入敏感詞
	ac := NewAhoCorasick()
	for _, word := range sensitiveWords {
		ac.Insert(word)
	}
	ac.Build()

	// 模擬消息處理
	message := "這是一條敏感詞測試消息，包含了死廢物和混蛋。"
	results := ac.Filter(message)
	for word, count := range results {
		fmt.Printf("檢測到敏感詞: %s (次數: %d)\n", word, count)
	}

	// 將檢測到的敏感詞替換為 *
	filteredMessage := message
	for word := range results {
		replacement := strings.Repeat("*", len(word))
		filteredMessage = strings.ReplaceAll(filteredMessage, word, replacement)
	}
	fmt.Println("Filtered message:", filteredMessage)
}
