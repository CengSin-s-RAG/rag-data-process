package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/google/uuid"
	"github.com/kydenul/markdown-chunker"
	"github.com/pgvector/pgvector-go"
	"github.com/sashabaranov/go-openai"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

const vectorSize = 1536

// IvankaContent 历史数据表 ivanka_content（与 main 分支 article_entries 结构对齐）
type ArticleEntries struct {
	Id           int64
	Title        string
	ContentShort string
	Content      string
	CreatedAt    time.Time
}

// VectorStore 向量表 vector_stores，存储 chunk 及嵌入向量
type VectorStore struct {
	Id          string          `gorm:"type:uuid;primaryKey"`
	Embedding   pgvector.Vector `gorm:"type:vector(1536);not null"`
	TextToIndex string          `gorm:"column:text_to_index;type:text"`
	Title       string          `gorm:"type:text"`
	CreatedAt   int64           `gorm:"column:created_at;type:bigint"`
	SourceId    int64           `gorm:"column:source_id;type:bigint"`
	ChunkIndex  int             `gorm:"column:chunk_index;type:int"`
	Summary     string          `gorm:"type:text"`
}

func (VectorStore) TableName() string { return "vector_stores" }

func (e *ArticleEntries) GetPayload(chunkIndex int, input string) (textToIndex, title, summary string, createdAt int64, sourceId int64, chunkIdx int) {
	return strings.ToValidUTF8(input, ""), e.Title, e.ContentShort, e.CreatedAt.Unix(), e.Id, chunkIndex
}

func main() {
	ctx := context.Background()
	runtime.GOMAXPROCS(runtime.NumCPU() / 2)

	apiKey := os.Getenv("OPENROUTER_API_KEY")
	baseURL := os.Getenv("OPENROUTER_API_BASE_URL")
	if apiKey == "" || baseURL == "" {
		log.Fatalln("需要环境变量: OPENROUTER_API_KEY, OPENROUTER_API_BASE_URL")
	}

	path, _ := filepath.Abs("offset.txt")
	fmt.Println("offset file path =", path)

	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0666)
	if err != nil {
		panic(err)
	}
	lastID := getOffsetFromFile(f)
	defer func() {
		saveOffsetToFile(f, lastID)
		f.Close()
	}()

	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		dsn = "host=localhost user=postgres password=postgres dbname=vectors_db port=5432 sslmode=disable TimeZone=Asia/Shanghai"
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		panic(err)
	}

	// 确保 pgvector 扩展并建表
	if err := db.Exec("CREATE EXTENSION IF NOT EXISTS vector").Error; err != nil {
		panic(fmt.Errorf("创建 pgvector 扩展失败: %w", err))
	}
	if err := db.AutoMigrate(&VectorStore{}); err != nil {
		panic(fmt.Errorf("迁移 vector_stores 表失败: %w", err))
	}

	conf := openai.DefaultConfig(apiKey)
	conf.BaseURL = baseURL
	client := openai.NewClientWithConfig(conf)

	for {
		var rows []*ArticleEntries
		if err = db.Table("article_entries").Select("id, title, content_short, content, created_at").
			Where("id > ?", lastID).
			Order("id asc").
			Limit(100).Find(&rows).Error; err != nil {
			panic(fmt.Errorf("查询 ivanka_content 失败: %w", err))
		}
		if len(rows) == 0 {
			break
		}

		for _, row := range rows {
			md, err := getInputPG(row)
			if err != nil {
				panic(fmt.Errorf("解析内容失败: %w", err))
			}
			chunks, err := chunk(md)
			if err != nil {
				panic(fmt.Errorf("chunk 失败: %w", err))
			}

			var vectors [][]float32
			for _, input := range chunks {
				chatCompletion, err := client.CreateEmbeddings(ctx, openai.EmbeddingRequest{
					Input:          convertPG(row, row.Title+"\n\n"+row.ContentShort+"\n\n"+input.Text),
					Model:          "qwen/qwen3-embedding-8b",
					EncodingFormat: openai.EmbeddingEncodingFormatFloat,
					Dimensions:     vectorSize,
				})
				if err != nil {
					panic(err)
				}
				if len(chatCompletion.Data) == 0 {
					continue
				}
				vectors = append(vectors, chatCompletion.Data[0].Embedding)
			}

			for i, vec := range vectors {
				if i >= len(chunks) {
					break
				}
				textToIndex, title, summary, createdAt, sourceId, chunkIdx := row.GetPayload(i, chunks[i].Text)
				vs := &VectorStore{
					Id:          uuid.New().String(),
					Embedding:   pgvector.NewVector(vec),
					TextToIndex: textToIndex,
					Title:       title,
					CreatedAt:   createdAt,
					SourceId:    sourceId,
					ChunkIndex:  chunkIdx,
					Summary:     summary,
				}
				if err := db.Create(vs).Error; err != nil {
					panic(fmt.Errorf("写入 vector_stores 失败: %w", err))
				}
			}
			lastID = row.Id
			fmt.Println("upserted row id", lastID)
		}
	}
}

func chunk(md string) ([]markdownchunker.Chunk, error) {
	config := markdownchunker.DefaultConfig()
	config.MaxChunkSize = 1000
	chunker := markdownchunker.NewMarkdownChunkerWithConfig(config)
	return chunker.ChunkDocument([]byte(md))
}

func convertPG(row *ArticleEntries, input string) string {
	prefix := "付鹏"
	if strings.Contains(row.Title, prefix) ||
		strings.Contains(row.ContentShort, prefix) ||
		strings.Contains(input, prefix) {
		prefix = ""
	}
	return fmt.Sprintf("%s\n\n%s\n\n%s", row.Title, row.ContentShort, input)
}

func getInputPG(row *ArticleEntries) (string, error) {
	return htmltomarkdown.ConvertString(row.Content)
}

func saveOffsetToFile(f *os.File, id int64) {
	_, _ = f.WriteString(fmt.Sprintf("\n%s:%d", time.Now().Format(time.DateOnly), id))
	_ = f.Sync()
}

func getOffsetFromFile(f *os.File) int64 {
	f.Seek(0, 0)
	all, err := io.ReadAll(f)
	if err != nil {
		panic(err)
	}
	content := strings.TrimSpace(string(all))
	if content == "" {
		return 0
	}
	rows := strings.Split(content, "\n")
	last := ""
	for i := len(rows) - 1; i >= 0; i-- {
		if strings.TrimSpace(rows[i]) != "" {
			last = rows[i]
			break
		}
	}
	if last == "" {
		return 0
	}
	tags := strings.Split(last, ":")
	if len(tags) < 2 {
		return 0
	}
	id, err := strconv.ParseInt(tags[1], 10, 64)
	if err != nil {
		return 0
	}
	return id
}
