package main

import (
	"context"
	"fmt"
	"github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/google/uuid"
	"github.com/kydenul/markdown-chunker"
	"github.com/qdrant/go-client/qdrant"
	"github.com/sashabaranov/go-openai"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type ArticleEntries struct {
	Id           int64
	Title        string
	ContentShort string
	Content      string
	CreatedAt    time.Time
}

func (e *ArticleEntries) GetPayload(chunkIndex int, input string) map[string]any {
	return map[string]any{
		"textToIndex": strings.ToValidUTF8(input, ""),
		"title":       e.Title,
		"created_at":  e.CreatedAt.Unix(),
		"id":          e.Id,
		"chunk_index": chunkIndex,
		"summary":     e.ContentShort,
	}
}

func main() {
	ctx := context.Background()
	runtime.GOMAXPROCS(runtime.NumCPU() / 2)

	var (
		collectionName = os.Getenv("COLLECTION_NAME")
		apiKey         = os.Getenv("OPENROUTER_API_KEY")
		bashUrl        = os.Getenv("OPENROUTER_API_BASE_URL")
		VectorSize     = 1536
	)

	path, _ := filepath.Abs("offset.txt")
	fmt.Println("offset file path =", path)

	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0666)
	if err != nil {
		panic(err)
	}

	lastID := getOffsetFromFile(f) // 获取断点位置
	defer func() {
		saveOffsetToFile(f, lastID)
		f.Close()
	}() // 实时记录断点
	// 构建 DSN 连接字符串
	dsn := fmt.Sprintf("root:rootpassword@tcp(localhost:3306)/ivanka_content?charset=utf8mb4&parseTime=True")
	// 初始化 MySQL 连接
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		panic(err)
	}

	conf := openai.DefaultConfig(apiKey)
	conf.BaseURL = bashUrl
	client := openai.NewClientWithConfig(conf)

	// 初始化 Qdrant 连接
	qdrantClient, err := qdrant.NewClient(&qdrant.Config{
		Host: "localhost",
		Port: 6334,
	})
	if err != nil {
		log.Fatalln("qdrant client init failed, err ", err)
	}
	defer qdrantClient.Close()

	cols, err := qdrantClient.ListCollections(ctx)
	if err != nil {
		log.Fatalln(err.Error())
	}

	colExist := false
	for _, col := range cols {
		if collectionName == col {
			colExist = true
			break
		}
	}

	if !colExist {
		// 3. 创建集合

		err = qdrantClient.CreateCollection(ctx, &qdrant.CreateCollection{
			CollectionName: collectionName,
			VectorsConfig: qdrant.NewVectorsConfig(&qdrant.VectorParams{
				Size:     uint64(VectorSize),
				Distance: qdrant.Distance_Cosine, // 余弦相似度
			}),
		})

		if err != nil {
			log.Fatalln("create collection failed, err ", err)
		}
		// 4. 创建 Payload 索引 (为了高性能过滤)
		// Qdrant 的 Go SDK 稍微有些底层，需要操作 PointsClient
		createIndex(ctx, qdrantClient, collectionName, "created_at", qdrant.FieldType_FieldTypeInteger)
		createIndex(ctx, qdrantClient, collectionName, "summary", qdrant.FieldType_FieldTypeText)
		createIndex(ctx, qdrantClient, collectionName, "textToIndex", qdrant.FieldType_FieldTypeText)
		createIndex(ctx, qdrantClient, collectionName, "title", qdrant.FieldType_FieldTypeText)
		createIndex(ctx, qdrantClient, collectionName, "id", qdrant.FieldType_FieldTypeInteger)
	}

	for {
		var rows []*ArticleEntries
		if err = db.Table("article_entries").Select("id, title, content_short, content, created_at").
			Where("id > ?", lastID).
			Order("id asc").
			Limit(100).Find(&rows).Error; err != nil {
			break
		}

		if len(rows) == 0 {
			break
		}

		for _, row := range rows {
			md, err := getInput(row)
			if err != nil {
				panic(fmt.Errorf("failed to parse input: %w", err))
			}
			// chunk
			chunks, err := chunk(md)
			if err != nil {
				panic(fmt.Errorf("failed to chunk input: %w", err))
			}

			var vectors [][]float32
			for _, input := range chunks {
				// 向量化
				chatCompletion, err := client.CreateEmbeddings(ctx, openai.EmbeddingRequest{
					Input:          convert(row, row.Title+"\n\n"+row.ContentShort+"\n\n"+input.Text),
					Model:          "qwen/qwen3-embedding-8b",
					EncodingFormat: openai.EmbeddingEncodingFormatFloat,
					Dimensions:     VectorSize,
				})

				if err != nil {
					panic(err)
				}
				if len(chatCompletion.Data) == 0 {
					continue
				}

				vec := chatCompletion.Data[0].Embedding
				vectors = append(vectors, vec)
			}

			// 保存到qdrant中
			// todo 增减数据权限要求，比如存储可以访问此数据的用户角色
			var points []*qdrant.PointStruct
			for i, vec := range vectors {
				point := &qdrant.PointStruct{
					Id:      qdrant.NewID(uuid.New().String()),
					Vectors: qdrant.NewVectors(vec...),
					Payload: qdrant.NewValueMap(row.GetPayload(i, chunks[i].Text)),
				}

				points = append(points, point)
			}

			wait := true
			_, err = qdrantClient.Upsert(ctx, &qdrant.UpsertPoints{
				CollectionName: collectionName,
				Points:         points,
				Wait:           &wait,
			})
			if err != nil {
				panic(fmt.Errorf("failed to upsert point: %w", err))
			}
			lastID = row.Id
			fmt.Println("upserted row id", lastID)
		}
	}
}

func createIndex(ctx context.Context, client *qdrant.Client, collectionName, fieldName string, fieldType qdrant.FieldType) {
	_, err := client.CreateFieldIndex(ctx, &qdrant.CreateFieldIndexCollection{
		CollectionName: collectionName,
		FieldName:      fieldName,
		FieldType:      &fieldType,
	})
	if err != nil {
		// 忽略"索引已存在"的错误，简化逻辑
		fmt.Printf("⚠️  创建索引 '%s' 时提示 (可能是已存在): %v\n", fieldName, err)
	} else {
		fmt.Printf("✅ 索引 '%s' 创建成功\n", fieldName)
	}
}

func chunk(md string) ([]markdownchunker.Chunk, error) {
	config := markdownchunker.DefaultConfig()
	config.MaxChunkSize = 1000
	chunker := markdownchunker.NewMarkdownChunkerWithConfig(config)
	chunks, err := chunker.ChunkDocument([]byte(md))
	if err != nil {
		return nil, err
	}

	return chunks, nil
}

func convert(row *ArticleEntries, input string) string {
	prefix := "付鹏"
	if strings.Contains(row.Title, prefix) ||
		strings.Contains(row.ContentShort, prefix) ||
		strings.Contains(input, prefix) {
		prefix = ""
	}

	return fmt.Sprintf("%s\n\n%s\n\n%s", row.Title, row.ContentShort, input)
}

func getInput(row *ArticleEntries) (string, error) {
	convertString, err := htmltomarkdown.ConvertString(row.Content)
	if err != nil {
		return "", err
	}

	return convertString, nil
}

func saveOffsetToFile(f *os.File, id int64) {
	_, err := f.WriteString(fmt.Sprintf("\n%s:%d", time.Now().Format(time.DateOnly), id))
	if err != nil {
		fmt.Println(err)
	}

	if err = f.Sync(); err != nil {
		fmt.Println(err)
	}
}

func getOffsetFromFile(f *os.File) int64 {
	// 重置文件指针到开头，否则多次读会读不到内容
	f.Seek(0, 0)
	all, err := io.ReadAll(f)
	if err != nil {
		panic(err)
	}

	content := string(all)
	if len(strings.TrimSpace(content)) == 0 {
		return 0
	}

	rows := strings.Split(content, "\n")
	if len(rows) == 0 {
		return 0
	}
	// 找到最后一个非空行
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
