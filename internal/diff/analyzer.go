package diff

import (
	"fmt"
	"os/exec"
	"regexp"
	"sort"

	//  "strconv"
	"strings"
)

type FileAnalysis struct {
    Path     string
    Priority int
    Tokens   int
    Category string
    Size     int
}

type Analyzer interface {
    GetPRDiff(baseRef, headRef string) (string, error)
    AnalyzeAndPrioritize(fullDiff string, baseRef, headRef string) (string, error)
    AnalyzeFiles(baseRef, headRef string) ([]FileAnalysis, error)
    BuildDiffFromAnalyses(analyses []FileAnalysis, baseRef, headRef string) (string, error)
}

type diffAnalyzer struct {
    maxTokens       int
    safeTokens      int
    ignorePatterns  []string
    includePatterns []string
}

func NewDiffAnalyzer(maxTokens int, ignorePatterns, includePatterns []string) Analyzer {
    return &diffAnalyzer{
        maxTokens:       maxTokens,
        safeTokens:      maxTokens - 1000, // Conservative buffer
        ignorePatterns:  ignorePatterns,
        includePatterns: includePatterns,
    }
}

func (da *diffAnalyzer) GetPRDiff(baseRef, headRef string) (string, error) {
    // Fetch the base branch
    fmt.Printf("🔄 Fetching base branch: %s\n", baseRef)
    cmd := exec.Command("git", "fetch", "origin", baseRef)
    if output, err := cmd.CombinedOutput(); err != nil {
        return "", fmt.Errorf("failed to fetch base branch: %w (output: %s)", err, string(output))
    }

    // Fetch the head branch too
    fmt.Printf("🔄 Fetching head branch: %s\n", headRef)
    cmd = exec.Command("git", "fetch", "origin", headRef)
    if output, err := cmd.CombinedOutput(); err != nil {
        return "", fmt.Errorf("failed to fetch head branch: %w (output: %s)", err, string(output))
    }

    // Get the diff - FIX: Use proper remote branch references
    fmt.Printf("🔄 Getting diff: origin/%s...origin/%s\n", baseRef, headRef)
    cmd = exec.Command("git", "diff", fmt.Sprintf("origin/%s...origin/%s", baseRef, headRef))
    output, err := cmd.CombinedOutput()
    if err != nil {
        return "", fmt.Errorf("failed to get diff: %w (output: %s)", err, string(output))
    }

    return string(output), nil
}

func (da *diffAnalyzer) AnalyzeAndPrioritize(fullDiff string, baseRef, headRef string) (string, error) {
    estimatedTokens := len(fullDiff) / 4

    if estimatedTokens <= da.maxTokens {
        return fullDiff, nil
    }

    analyses, err := da.AnalyzeFiles(baseRef, headRef)
    if err != nil {
        return "", err
    }

    // Build prioritized diff
    return da.buildPrioritizedDiff(analyses, baseRef, headRef)
}

func (da *diffAnalyzer) AnalyzeFiles(baseRef, headRef string) ([]FileAnalysis, error) {
    // Get changed files
    changedFiles, err := da.getChangedFiles(baseRef, headRef)
    if err != nil {
        return nil, err
    }

    // Analyze each file
    analyses := make([]FileAnalysis, 0, len(changedFiles))
    for _, file := range changedFiles {
        analysis, err := da.analyzeFile(file, baseRef, headRef)
        if err != nil {
            continue // Skip problematic files
        }
        if da.shouldIncludeFile(analysis.Path) {
            analyses = append(analyses, analysis)
        }
    }

    // Sort by priority, then by size
    sort.Slice(analyses, func(i, j int) bool {
        if analyses[i].Priority != analyses[j].Priority {
            return analyses[i].Priority < analyses[j].Priority
        }
        return analyses[i].Tokens < analyses[j].Tokens
    })

    return analyses, nil
}

func (da *diffAnalyzer) getChangedFiles(baseRef, headRef string) ([]string, error) {
    // FIX: Use consistent remote branch references
    cmd := exec.Command("git", "diff", "--name-only", fmt.Sprintf("origin/%s...origin/%s", baseRef, headRef))
    output, err := cmd.CombinedOutput()
    if err != nil {
        return nil, fmt.Errorf("failed to get changed files: %w (output: %s)", err, string(output))
    }

    files := strings.Split(strings.TrimSpace(string(output)), "\n")
    var validFiles []string
    for _, file := range files {
        if file != "" {
            validFiles = append(validFiles, file)
        }
    }
    return validFiles, nil
}

func (da *diffAnalyzer) analyzeFile(filepath, baseRef, headRef string) (FileAnalysis, error) {
    // FIX: Use consistent remote branch references
    cmd := exec.Command("git", "diff", fmt.Sprintf("origin/%s...origin/%s", baseRef, headRef), "--", filepath)
    output, err := cmd.CombinedOutput()
    if err != nil {
        return FileAnalysis{}, fmt.Errorf("failed to analyze file %s: %w (output: %s)", filepath, err, string(output))
    }

    size := len(output)
    tokens := size / 4
    priority, category := da.categorizeFile(filepath)

    return FileAnalysis{
        Path:     filepath,
        Priority: priority,
        Tokens:   tokens,
        Category: category,
        Size:     size,
    }, nil
}

func (da *diffAnalyzer) categorizeFile(filepath string) (int, string) {
    // Core application files - Priority 1
    corePatterns := []string{
        `\.(js|jsx|ts|tsx|py|java|go|rs|php|rb|swift|kt|scala|cs|cpp|c|h|hpp)$`,
    }
    testPatterns := []string{`(test|spec|mock)`}
    
    for _, pattern := range corePatterns {
        if matched, _ := regexp.MatchString(pattern, filepath); matched {
            // Check if it's a test file
            isTest := false
            for _, testPattern := range testPatterns {
                if matched, _ := regexp.MatchString(testPattern, filepath); matched {
                    isTest = true
                    break
                }
            }
            if !isTest {
                return 1, "Core Code"
            }
        }
    }

    // Configuration files - Priority 2
    configPatterns := []string{
        `package\.json|package-lock\.json|requirements\.txt|Dockerfile|docker-compose\.yml`,
        `\.env|\.env\.|config\.|\.config|\.yml$|\.yaml$|\.toml$|\.ini$|\.conf$`,
        `Makefile|CMakeLists\.txt`,
    }
    for _, pattern := range configPatterns {
        if matched, _ := regexp.MatchString(pattern, filepath); matched {
            return 2, "Configuration"
        }
    }

    // Tests and docs - Priority 3
    testDocPatterns := []string{
        `\.(test\.|spec\.|_test\.py|test_.*\.py)$`,
        `README|CHANGELOG|LICENSE`,
    }
    for _, pattern := range testDocPatterns {
        if matched, _ := regexp.MatchString(pattern, filepath); matched {
            return 3, "Tests/Docs"
        }
    }

    // Styles and medium docs - Priority 4
    stylePatterns := []string{
        `\.(css|scss|sass|less|styl|md|rst|txt)$`,
    }
    for _, pattern := range stylePatterns {
        if matched, _ := regexp.MatchString(pattern, filepath); matched {
            return 4, "Styles/Docs"
        }
    }

    return 5, "Other"
}

func (da *diffAnalyzer) shouldIncludeFile(filepath string) bool {
    // Check ignore patterns
    for _, pattern := range da.ignorePatterns {
        if pattern != "" {
            if matched, _ := regexp.MatchString(pattern, filepath); matched {
                return false
            }
        }
    }

    // Check include patterns (if specified)
    if len(da.includePatterns) > 0 && da.includePatterns[0] != "" {
        for _, pattern := range da.includePatterns {
            if matched, _ := regexp.MatchString(pattern, filepath); matched {
                return true
            }
        }
        return false
    }

    return true
}

func (da *diffAnalyzer) BuildDiffFromAnalyses(analyses []FileAnalysis, baseRef, headRef string) (string, error) {
    return da.buildPrioritizedDiff(analyses, baseRef, headRef)
}

func (da *diffAnalyzer) buildPrioritizedDiff(analyses []FileAnalysis, baseRef, headRef string) (string, error) {
    var result strings.Builder
    currentTokens := 150 // Header overhead
    filesIncluded := 0
    categoryCount := make(map[string]int)

    // Header
    result.WriteString("# Focused Code Review - Smart File Prioritization\n\n")
    result.WriteString("**Analysis Strategy:** Prioritizing critical code files, configurations, and tests within token constraints.\n\n")
    result.WriteString("## Files Included in Review:\n\n")

    for _, analysis := range analyses {
        if currentTokens + analysis.Tokens < da.safeTokens {
            // Add file section
            result.WriteString(fmt.Sprintf("### %s: `%s`\n", analysis.Category, analysis.Path))
            result.WriteString(fmt.Sprintf("**Priority:** %d | **Estimated Impact:** %d tokens\n\n", analysis.Priority, analysis.Tokens))

            // FIX: Get file diff with consistent references
            cmd := exec.Command("git", "diff", fmt.Sprintf("origin/%s...origin/%s", baseRef, headRef), "--", analysis.Path)
            output, err := cmd.CombinedOutput()
            if err == nil {
                result.Write(output)
                result.WriteString("\n\n---\n\n")
            } else {
                // Log the error but continue
                fmt.Printf("⚠️  Failed to get diff for %s: %s\n", analysis.Path, err.Error())
            }

            currentTokens += analysis.Tokens
            filesIncluded++
            categoryCount[analysis.Category]++
        }
    }

    // Footer summary
    result.WriteString("\n## Review Scope Summary\n")
    result.WriteString("| Category | Files Reviewed |\n")
    result.WriteString("|----------|----------------|\n")
    for category, count := range categoryCount {
        result.WriteString(fmt.Sprintf("| %s | %d |\n", category, count))
    }
    result.WriteString(fmt.Sprintf("\n**Total Files:** %d | **Token Usage:** ~%d/%d\n", filesIncluded, currentTokens, da.maxTokens))

    finalResult := result.String()
    
    // Final safety truncation
    finalTokens := len(finalResult) / 4
    if finalTokens > da.safeTokens {
        // Hard truncation like in YAML
        maxBytes := da.safeTokens * 4
        if len(finalResult) > maxBytes {
            truncated := finalResult[:maxBytes]
            truncated += "\n\n--- DIFF TRUNCATED: REACHED SAFE TOKEN LIMIT ---\n"
            truncated += "**Remaining files require separate review**\n"
            return truncated, nil
        }
    }
    
    return finalResult, nil
}