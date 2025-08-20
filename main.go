package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/euclidstellar/code-review-agent/internal/config"
	"github.com/euclidstellar/code-review-agent/internal/diff"
	"github.com/euclidstellar/code-review-agent/internal/github"
	"github.com/euclidstellar/code-review-agent/internal/reviewer"
	"github.com/euclidstellar/code-review-agent/internal/utils"
)

const (
	LargePRMinTokens         = 12000
	PrioritizedReviewMaxTokens = 12400
	ChunkTargetTokens        = 6200
)

func main() {
	logger := utils.NewLogger()
	if err := run(logger); err != nil {
		logger.Error("Action failed: %v", err)
		os.Exit(1)
	}
	logger.Info("✅ Action completed successfully.")
}

func run(logger *utils.Logger) error {
	ctx := context.Background()
	logger.Info("Starting Smart AI Code Review Action v1.0.0")

	// Fix git ownership issue first
	setupGitSafeDirectory(logger)

	// Load configuration
	cfg, err := config.LoadFromEnv()
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}
	logger.Info("Configuration loaded: Model=%s, MaxTokens=%d, OllamaFallback=%v",
		cfg.Model, cfg.MaxTokens, cfg.UseOllamaFallback)

	// Initialize clients
	ghClient := github.NewClient(cfg.GitHubToken, cfg.RepoOwner, cfg.RepoName)
	diffAnalyzer := diff.NewDiffAnalyzer(cfg.MaxTokens, cfg.IgnorePatterns, cfg.IncludePatterns)

	// Get and analyze PR diff
	logger.Info("Fetching PR diff for %s...%s", cfg.BaseRef, cfg.HeadRef)
	prDiff, err := diffAnalyzer.GetPRDiff(cfg.BaseRef, cfg.HeadRef)
	if err != nil {
		return fmt.Errorf("failed to get PR diff: %w", err)
	}
	if prDiff == "" {
		logger.Info("No changes found, skipping review.")
		logger.GitHubOutput("review-posted", "false")
		return nil
	}

	estimatedTokens := len(prDiff) / 4
	logger.Info("Estimated total diff tokens: ~%d", estimatedTokens)

	// New logic for large PRs: if total tokens exceed the minimum, handle with chunking.
	if estimatedTokens >= LargePRMinTokens {
		logger.Info("Large PR detected. Performing prioritized, chunked review.")
		return handleMultiReview(ctx, cfg, ghClient, diffAnalyzer, logger)
	}

	// --- Existing Review Logic for smaller PRs ---
	logger.Info("Analyzing diff (original size: %d bytes)", len(prDiff))
	prioritizedDiff, err := diffAnalyzer.AnalyzeAndPrioritize(prDiff, cfg.BaseRef, cfg.HeadRef)
	if err != nil {
		return fmt.Errorf("failed to analyze diff: %w", err)
	}
	logger.Info("Prioritized diff size: %d bytes (~%d tokens)", len(prioritizedDiff), len(prioritizedDiff)/4)

	review, provider, err := generateReview(ctx, cfg, prioritizedDiff, ghClient, logger)
	if err != nil {
		logger.Error("All review providers failed. Posting static fallback. Final error: %v", err)
		review = generateFallbackReview(prioritizedDiff, err.Error())
		provider = "static-fallback"
	}

	// Post the final review comment
	if err := ghClient.PostComment(ctx, cfg.PRNumber, review); err != nil {
		return fmt.Errorf("failed to post final comment: %w", err)
	}

	logger.Info("Posted review using provider: %s", provider)
	logger.GitHubOutput("review-posted", "true")
	logger.GitHubOutput("review-provider", provider)
	return nil
}

func handleMultiReview(ctx context.Context, cfg *config.Config, ghClient *github.Client, diffAnalyzer diff.Analyzer, logger *utils.Logger) error {
	// 1. Get all changed files, sorted by priority.
	allAnalyses, err := diffAnalyzer.AnalyzeFiles(cfg.BaseRef, cfg.HeadRef)
	if err != nil {
		return fmt.Errorf("failed to analyze files for chunking: %w", err)
	}

	// 2. Select a prioritized subset of files up to PrioritizedReviewMaxTokens.
	prioritizedAnalyses := []diff.FileAnalysis{}
	currentTokens := 0
	for _, analysis := range allAnalyses {
		if currentTokens+analysis.Tokens > PrioritizedReviewMaxTokens {
			break // Stop once we exceed the total limit for the prioritized review.
		}
		prioritizedAnalyses = append(prioritizedAnalyses, analysis)
		currentTokens += analysis.Tokens
	}
	logger.Info("Selected %d high-priority files with a total of ~%d tokens for review.", len(prioritizedAnalyses), currentTokens)

	// 3. Split the prioritized files into exactly two chunks.
	chunks := splitIntoTwoChunks(prioritizedAnalyses, ChunkTargetTokens)
	logger.Info("Split prioritized files into %d chunks.", len(chunks))

	for i, chunk := range chunks {
		logger.Info("Processing chunk %d/%d with %d files (~%d tokens)...", i+1, len(chunks), len(chunk.Analyses), chunk.TotalTokens)

		diff, err := diffAnalyzer.BuildDiffFromAnalyses(chunk.Analyses, cfg.BaseRef, cfg.HeadRef)
		if err != nil {
			logger.Error("Failed to build diff for chunk %d: %v. Skipping.", i+1, err)
			continue
		}

		review, provider, err := generateReview(ctx, cfg, diff, ghClient, logger)
		if err != nil {
			logger.Error("Failed to generate review for chunk %d: %v. Posting fallback.", i+1, err)
			review = generateFallbackReview(diff, err.Error())
			provider = "static-fallback"
		}

		// Add a header to each review comment
		reviewHeader := fmt.Sprintf("## 🧩 Prioritized Code Review (Part %d/%d)\n\n", i+1, len(chunks))
		finalReview := reviewHeader + review

		if err := ghClient.PostComment(ctx, cfg.PRNumber, finalReview); err != nil {
			logger.Error("Failed to post comment for chunk %d: %v", i+1, err)
		} else {
			logger.Info("Successfully posted review for chunk %d using %s.", i+1, provider)
		}
	}

	logger.GitHubOutput("review-posted", "true")
	logger.GitHubOutput("review-provider", "github-models/chunked")
	return nil
}

type AnalysisChunk struct {
	Analyses    []diff.FileAnalysis
	TotalTokens int
}

// splitIntoTwoChunks divides a list of file analyses into exactly two chunks.
// The first chunk is filled up to the target token size, and the second gets the rest.
func splitIntoTwoChunks(analyses []diff.FileAnalysis, targetTokens int) []AnalysisChunk {
	if len(analyses) == 0 {
		return []AnalysisChunk{}
	}

	chunk1 := AnalysisChunk{}
	
	splitIndex := 0
	for i, analysis := range analyses {
		if chunk1.TotalTokens > 0 && chunk1.TotalTokens+analysis.Tokens > targetTokens && i > 0 {
			break
		}
		chunk1.Analyses = append(chunk1.Analyses, analysis)
		chunk1.TotalTokens += analysis.Tokens
		splitIndex = i + 1
	}

	// If all files fit into the first chunk, or there's only one file
	if splitIndex >= len(analyses) {
		return []AnalysisChunk{chunk1}
	}

	chunk2 := AnalysisChunk{}
	chunk2.Analyses = analyses[splitIndex:]
	for _, analysis := range chunk2.Analyses {
		chunk2.TotalTokens += analysis.Tokens
	}

	return []AnalysisChunk{chunk1, chunk2}
}

func generateReview(ctx context.Context, cfg *config.Config, diff string, ghClient *github.Client, logger *utils.Logger) (string, string, error) {
	// Attempt 1: GitHub Models
	logger.Info("🤖 Attempting review with GitHub Models (%s)...", cfg.Model)
	ghReview, err := tryGitHubModels(cfg, diff, logger)
	if err == nil {
		return ghReview, "github-models", nil
	}
	logger.Error("GitHub Models failed: %v", err)

	// Check if it's a token limit error
	if errors.Is(err, config.ErrTokenLimitExceeded) {
		// Post the friendly "coffee" message
		coffeeMessage := "Hey, it looks like your PR diff is very big, but don't worry, we got you! Grab a coffee, and before you finish it, your PR review will be ready. ☕"
		if postErr := ghClient.PostComment(ctx, cfg.PRNumber, coffeeMessage); postErr != nil {
			logger.Error("Failed to post 'coffee' comment: %v", postErr)
		}
	}

	// Attempt 2: Ollama Fallback
	if cfg.UseOllamaFallback {
		logger.Info("🔄 Attempting review with Ollama fallback (%s)...", cfg.OllamaModel)
		ollamaReview, err := tryOllamaFallback(ctx, cfg, diff, logger)
		if err == nil {
			return ollamaReview, "ollama", nil
		}
		logger.Error("Ollama fallback also failed: %v", err)
		return "", "", err // Return the last error
	}

	return "", "", err // Return the original error if Ollama is disabled
}

func tryGitHubModels(cfg *config.Config, diff string, logger *utils.Logger) (string, error) {
	logger.Info("🤖 Generating review with GitHub Models (%s)", cfg.Model)

	aiClient := reviewer.NewGitHubModelsClient(cfg.ModelsToken)
	review, err := aiClient.GenerateReview(diff, cfg.Model, cfg.Temperature, 4000)

	if err != nil {
		// Check for our specific token limit error
		if errors.Is(err, config.ErrTokenLimitExceeded) {
			return "", err // Pass the specific error up
		}
		// Check for other rate limit or quota errors
		if isRateLimitError(err) || isQuotaError(err) {
			return "", fmt.Errorf("rate limit/quota exceeded: %w", err)
		}
		return "", err
	}

	return review, nil
}

func tryOllamaFallback(ctx context.Context, cfg *config.Config, diff string, logger *utils.Logger) (string, error) {
	logger.Info("🦙 Setting up Ollama with model %s", cfg.OllamaModel)

	ollamaClient := reviewer.NewOllamaClient(cfg.OllamaModel)
	defer ollamaClient.Cleanup()

	// Setup Ollama (install, start service, pull model)
	if err := ollamaClient.SetupOllama(ctx); err != nil {
		return "", fmt.Errorf("failed to setup Ollama: %w", err)
	}

	// Generate review
	logger.Info("🔄 Generating review with Ollama...")
	review, err := ollamaClient.GenerateReview(ctx, diff)
	if err != nil {
		return "", fmt.Errorf("failed to generate Ollama review: %w", err)
	}

	// Add Ollama header to review
	reviewWithHeader := fmt.Sprintf("## 🦙 Ollama Code Review (%s)\n\n> **Note:** This review was generated using Ollama as a fallback when GitHub Models was unavailable.\n\n%s", cfg.OllamaModel, review)

	return reviewWithHeader, nil
}

func isRateLimitError(err error) bool {
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "rate limit") ||
		strings.Contains(errStr, "too many requests") ||
		strings.Contains(errStr, "429")
}

func isQuotaError(err error) bool {
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "quota") ||
		strings.Contains(errStr, "usage limit") ||
		strings.Contains(errStr, "insufficient")
}

func setupGitSafeDirectory(logger *utils.Logger) {
	// Configure git to trust the workspace directory
	commands := [][]string{
		{"git", "config", "--global", "--add", "safe.directory", "/github/workspace"},
		{"git", "config", "--global", "--add", "safe.directory", "*"},
	}

	for _, cmd := range commands {
		if output, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput(); err != nil {
			fmt.Printf("⚠️  Warning: Could not configure git safe directory: %s\n", string(output))
		}
	}
	fmt.Println("✅ Git safe directory configured")
}

func generateFallbackReview(diff, errorMsg string) string {
	return fmt.Sprintf(`## ❌ AI Code Review - Error Occurred

An error occurred while generating the automated code review.

### Error Details
`+"```\n"+errorMsg+"\n```"+`

### Manual Review Required
Please proceed with manual code review using these guidelines:

#### Quick Review Checklist
- **Security:** Check for vulnerabilities and exposed credentials
- **Performance:** Look for inefficient code patterns  
- **Testing:** Ensure adequate test coverage
- **Documentation:** Verify code is properly documented
- **Standards:** Confirm adherence to team coding standards

#### Diff Statistics
- **Size:** %d bytes (~%d tokens)
- **Requires manual analysis due to processing error**

---
**Error Time:** %s  
**Suggested Action:** Manual review and investigate workflow configuration`,
		len(diff), len(diff)/4, "now")
}