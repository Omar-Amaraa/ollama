package convert

import (
	"bufio"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
	"math"
	"os"
	"strings"
	"unicode"

	"golang.org/x/exp/maps"
	"golang.org/x/exp/slices"
)

// ------------------------------------------------------------
// 📂 1.  Embedding de la liste de mots interdits
// ------------------------------------------------------------

//go:embed liste_a_filtrer.txt
var vocabData embed.FS

const (
	_ int32 = iota
	tokenTypeNormal
	tokenTypeUnknown
	tokenTypeControl
	tokenTypeUserDefined
	tokenTypeUnused
	tokenTypeByte
)

type TrieNode struct {
	children map[rune]*TrieNode
	isEnd    bool
}

type Tokenizer struct {
	*Vocabulary
	SpecialVocabulary []*SpecialVocabulary
	Merges            []string
	Pre               string
	Template          string
}
type tokenizer struct {
	AddedTokens []token `json:"added_tokens"`
	Model       struct {
		Type   string          `json:"type"`
		Vocab  map[string]int  `json:"vocab"`
		Merges json.RawMessage `json:"merges"`
	} `json:"model"`
	PreTokenizer struct {
		PreTokenizers []struct {
			Type    string `json:"type"`
			Pattern struct {
				Regex string `json:"Regex"`
			} `json:"pattern"`
		} `json:"pretokenizers"`
	} `json:"pre_tokenizer"`
}

func parseTokenizer(fsys fs.FS, specialTokenTypes []string) (*Tokenizer, error) {
	v, err := parseVocabulary(fsys)
	if err != nil {
		return nil, err
	}
	t := &Tokenizer{
		Vocabulary: v,
		Pre:        "default",
	}
	addedTokens := make(map[string]token)
	if f, err := fsys.Open("tokenizer.json"); errors.Is(err, os.ErrNotExist) {
	} else if err != nil {
		return nil, err
	} else {
		defer f.Close()
		var tt tokenizer
		if err := json.NewDecoder(f).Decode(&tt); err != nil {
			return nil, err
		}
		for _, t := range tt.AddedTokens {
			addedTokens[t.Content] = t
		}
		if len(tt.Model.Merges) == 0 {
			// noop; merges is empty
		} else if err := json.Unmarshal(tt.Model.Merges, &t.Merges); err == nil {
			// noop; merges is []string
		} else if merges, err := func() ([][]string, error) {
			var merges [][]string
			if err := json.Unmarshal(tt.Model.Merges, &merges); err != nil {
				return nil, err
			}
			return merges, nil
		}(); err == nil {
			t.Merges = make([]string, len(merges))
			for i := range merges {
				t.Merges[i] = strings.Join(merges[i], " ")
			}
		} else {
			return nil, fmt.Errorf("could not parse tokenizer merges. expected []string or [][]string: %w", err)
		}
		sha256sum := sha256.New()
		for _, pt := range tt.PreTokenizer.PreTokenizers {
			switch pt.Type {
			case "Split":
				if pt.Pattern.Regex != "" {
					// create a checksum of all Split pretokenizers which should be sufficient
					// to identify the pretokenizer
					sha256sum.Write([]byte(pt.Pattern.Regex))
				}
			}
		}
		switch digest := hex.EncodeToString(sha256sum.Sum(nil)); digest {
		case "d98f9631be1e9607a9848c26c1f9eac1aa9fc21ac6ba82a2fc0741af9780a48f":
			t.Pre = "llama-bpe"
		case "03df5c5863ad70781dcfdef491ead25140f895fe8010964be0daefe27be32b02":
			t.Pre = "deepseek-llm"
		case "21cde974d587f0d54dc8d56b183cc1e6239600172035c68fbd6d4b9f8da0576e":
			t.Pre = "deepseek-coder"
		case "1ff7f41064896984db5d1bb6ff64fa4bc29007d08c1b439e505b7392777a319e":
			t.Pre = "qwen2"
		case "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855":
			// noop, empty pretokenizer
		default:
			slog.Warn("unknown pretokenizer, using default", "digest", digest)
		}
	}
	if f, err := fsys.Open("tokenizer_config.json"); errors.Is(err, os.ErrNotExist) {
	} else if err != nil {
		return nil, err
	} else {
		defer f.Close()
		var p map[string]json.RawMessage
		if err := json.NewDecoder(f).Decode(&p); err != nil {
			return nil, err
		}
		if template, ok := p["chat_template"]; ok {
			var s []struct {
				Name     string `json:"name"`
				Template string `json:"template"`
			}
			if err := json.Unmarshal(template, &t.Template); err == nil {
				// noop
			} else if err := json.Unmarshal(template, &s); err == nil {
				for _, e := range s {
					if e.Name == "default" {
						t.Template = e.Template
						break
					}
				}
			} else {
				return nil, fmt.Errorf("invalid chat_template: %w", err)
			}
		}
		for _, st := range specialTokenTypes {
			sv := SpecialVocabulary{Type: st}
			if bts, ok := p[fmt.Sprintf("add_%s_token", st)]; ok {
				if err := json.Unmarshal(bts, &sv.AddToken); err != nil {
					return nil, err
				}
			}
			if bts, ok := p[fmt.Sprintf("%s_token", st)]; ok {
				var content string
				if err := json.Unmarshal(bts, &content); err != nil {
					var mm map[string]any
					if err := json.Unmarshal(bts, &mm); err != nil {
						continue
					}
					content, ok = mm["content"].(string)
					if !ok {
						continue
					}
				}
				sv.Content = content
			}
			if id, ok := addedTokens[sv.Content]; ok {
				sv.ID = id.ID
				t.SpecialVocabulary = append(t.SpecialVocabulary, &sv)
			}
		}
	}
	return t, nil
}

// Fonction pour charger les mots interdits depuis un fichier txt
/*func loadBannedWords(filename string) (map[string]bool, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	bannedWords := make(map[string]bool)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() { // parcours tous les lignes du fichier et les ajoute à la map
		word := scanner.Text()
		bannedWords[word] = true // source : https://stackoverflow.com/questions/38684841/reading-from-a-text-file-in-golang
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return bannedWords, nil
}*/

func NewTrieNode() *TrieNode {
	return &TrieNode{children: make(map[rune]*TrieNode)}
}

func addWordToTrie(root *TrieNode, word string) {
	node := root
	for _, ch := range word {
		if _, ok := node.children[ch]; !ok {
			node.children[ch] = NewTrieNode()
		}
		node = node.children[ch]
	}
	node.isEnd = true
}

var equivalents = map[rune]rune{
	'0': 'o', '1': 'i', '3': 'e', '4': 'a', '5': 's', '7': 't', '8': 'b',
	'$': 's', '@': 'a', '!': 'i',
}

func toLower(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + ('a' - 'A')
	}
	return r
}

func getEquivalent(ch rune) rune {
	ch = toLower(ch)
	if val, ok := equivalents[ch]; ok {
		return val
	}
	return ch
}
func checkPhraseInTrie(node *TrieNode, word string, index int) bool {
	if index == len(word) {
		return node.isEnd
	}
	ch := toLower(rune(word[index]))
	child, exists := node.children[ch]
	if !exists {
		return false
	}
	return checkPhraseInTrie(child, word, index+1)
}
func penalizedCheck(node *TrieNode, word string, index, s, i, threshold int) (bool, int) {
	if s > threshold {
		return false, s
	}
	if index == len(word) {
		return node.isEnd, s
	}
	ch := rune(word[index])
	eq := getEquivalent(ch)
	child, ok := node.children[eq]
	if !ok {
		return false, s
	}
	if eq == toLower(ch) {
		i = 2 // reset incrément
	} else {
		s += i * i
		i = s
	}
	return penalizedCheck(child, word, index+1, s, i, threshold)
}

func shouldEliminateWord(root *TrieNode, word string, threshold int) bool {
	found, _ := penalizedCheck(root, word, 0, 2, 2, threshold)
	return found
}
func loadBannedTrie() (*TrieNode, error) {
	root := NewTrieNode()

	// 6.1 Toujours interdire "mounib"
	addWordToTrie(root, "mounib")

	// 6.2 Parcours du fichier embarqué
	file, err := vocabData.Open("liste_a_filtrer.txt")
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		w := strings.ToLower(strings.TrimSpace(scanner.Text()))
		if w == "" {
			continue
		}
		addWordToTrie(root, w)
	}
	return root, scanner.Err()
}

// 📌 7.  Initialisation globale du Trie ‑ utilisable partout
var bannedTrie *TrieNode

func init() {
	var err error
	bannedTrie, err = loadBannedTrie()
	if err != nil {
		log.Fatalf("failed to load banned words: %v", err)
	}
}

// ------------------------------------------------------------
// 📂 8.  Filtrage au niveau de la phrase (NOUVEAU)
// ------------------------------------------------------------

// containsBannedPhrase renvoie true si la phrase contient un mot interdit
// ou suffisamment proche d'un mot interdit.
func containsBannedPhrase(root *TrieNode, sentence string) bool {
	text := normalize(sentence)
	for _, w := range strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r)
	}) {
		if checkPhraseInTrie(root, w, 0) {
			return true
		}
	}
	return false
}

// IsUnsafe permet d'appeler directement le filtre sur un texte généré.
func IsUnsafe(text string) bool {
	return containsBannedPhrase(bannedTrie, text)
}

/*
	func addTrieNode(parent TrieNode, word string) *TrieNode {
		fils := NewTrieNode()
		parent.children[word] = fils
		return fils
	}
*/

// Your existing constants, types, and functions...

type token struct {
	ID          int    `json:"id"`
	Content     string `json:"content"`
	Special     bool   `json:"special"`
	UserDefined bool
}
type Vocabulary struct {
	Model  string
	Tokens []string
	Scores []float32
	Types  []int32
}

func parseVocabularyFromTokenizer(fsys fs.FS, root *TrieNode) (*Vocabulary, error) {
	f, err := fsys.Open("tokenizer.json")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var t tokenizer
	if err := json.NewDecoder(f).Decode(&t); err != nil {
		return nil, err
	}

	tokens := make(map[int]token, len(t.Model.Vocab))

	for k, v := range t.Model.Vocab {
		log.Printf("Checking word: %s\n", k)
		if checkPhraseInTrie(root, k, 0) {
			log.Printf("Banned word detected in Trie: %s\n", k)
			continue
		}
		if shouldEliminateWord(root, k, 10) {
			log.Printf("Word similar to banned word detected: %s\n", k)
			continue
		}

		tokens[v] = token{
			ID:      v,
			Content: k,
		}
	}

	for _, token := range t.AddedTokens {
		token.UserDefined = true

		// Vérification stricte des mots interdits
		if checkPhraseInTrie(root, token.Content, 0) || shouldEliminateWord(root, token.Content, 10) {
			log.Printf("Mot interdit détecté dans AddedTokens et ignoré : %s\n", token.Content)
			continue
		}

		tokens[token.ID] = token
	}

	keys := maps.Keys(tokens)
	slices.Sort(keys)

	v := Vocabulary{Model: "gpt2"}
	for _, k := range keys {
		token := tokens[k]

		if checkPhraseInTrie(root, token.Content, 0) {
			continue
		}
		if shouldEliminateWord(root, token.Content, 10) {
			continue
		}

		v.Tokens = append(v.Tokens, token.Content)
		v.Scores = append(v.Scores, float32(token.ID))
		switch {
		case token.Special:
			v.Types = append(v.Types, tokenTypeControl)
		case token.UserDefined:
			v.Types = append(v.Types, tokenTypeUserDefined)
		default:
			v.Types = append(v.Types, tokenTypeNormal)
		}
	}

	return &v, nil
}

func parseVocabulary(fsys fs.FS) (*Vocabulary, error) {
	// 5.1 Charger la liste interdite
	rootTrie, err := loadBannedTrie()
	if err != nil {
		return nil, fmt.Errorf("failed to load banned words: %w", err)
	}

	// 5.2 Définir les patterns de tokenizer
	patterns := []struct {
		Pattern string
		Func    func(fs.FS, *TrieNode) (*Vocabulary, error)
	}{
		{"tokenizer.model", func(fsys fs.FS, _ *TrieNode) (*Vocabulary, error) {
			return parseSentencePiece(fsys)
		}}, // plus besoin de wrapper
		{"tokenizer.json", parseVocabularyFromTokenizer},
	}

	// 5.3 Parcourir les patterns
	for _, p := range patterns {
		if _, err := fsys.Open(p.Pattern); errors.Is(err, os.ErrNotExist) {
			continue // fichier absent → on essaie le suivant
		} else if err != nil {
			return nil, err // autre erreur I/O
		}
		// fichier trouvé → on parse et on renvoie
		return p.Func(fsys, rootTrie)
	}

	return nil, errors.New("unknown tokenizer format (aucun tokenizer.model ni tokenizer.json embarqué)")
}

type SpecialVocabulary struct {
	Type     string
	ID       int
	Content  string
	AddToken bool
}

func (sv SpecialVocabulary) Key() string {
	switch t := sv.Type; t {
	case "bos", "eos", "cls", "mask":
		return t
	case "unk":
		return "unknown"
	case "sep":
		//nolint:misspell // this is an upstream typo
		return "seperator"
	case "pad":
		return "padding"
	}
	panic("unknown special vocabulary type")
}

// Fonction qui calcule la distance de Levenshtein- source : https://en.wikipedia.org/wiki/Levenshtein_distance
// La distance de Levenshtein est une mesure de la différence entre deux chaînes de caractères
// Elle est définie comme le nombre minimum d'opérations d'édition (insertion, suppression ou substitution) nécessaires pour transformer une chaîne en une autre.
// Par exemple, la distance de Levenshtein entre "kitten" et "sitting" est 3, car il faut trois opérations pour transformer "kitten" en "sitting":
// 1. Remplacer "k" par "s"
// 2. Remplacer "e" par "i"
// 3. Ajouter "g" à la fin
// La distance de Levenshtein est souvent utilisée dans le traitement du langage naturel, la correction orthographique et la recherche de similarité entre chaînes de caractères.
// La complexité temporelle de l'algorithme est O(m*n), où m et n sont les longueurs des deux chaînes.
// La complexité spatiale est O(m*n) pour la matrice de distance, mais peut être optimisée à O(min(m,n)) en utilisant une seule ligne de la matrice à la fois.
// github : https://github.com/agnivade/levenshtein.git

func LevenshteinDistance(s1, s2 string) int {
	lenS1 := len(s1)
	lenS2 := len(s2)

	// Créer une matrice 2D (lenS1+1) x (lenS2+1)
	dp := make([][]int, lenS1+1)
	for i := range dp {
		dp[i] = make([]int, lenS2+1)
	}

	// Initialiser les bords
	for i := 0; i <= lenS1; i++ {
		dp[i][0] = i
	}
	for j := 0; j <= lenS2; j++ {
		dp[0][j] = j
	}

	// Remplir la matrice
	for i := 1; i <= lenS1; i++ {
		for j := 1; j <= lenS2; j++ {
			cost := 0
			if s1[i-1] != s2[j-1] {
				cost = 1
			}

			dp[i][j] = min(
				dp[i-1][j]+1,      // Suppression
				dp[i][j-1]+1,      // Insertion
				dp[i-1][j-1]+cost, // Substitution
			)
		}
	}

	return dp[lenS1][lenS2]
}

// Fonction utilitaire pour trouver le minimum de 3 entiers
func min(a, b, c int) int {
	return int(math.Min(float64(a), math.Min(float64(b), float64(c))))
}

func normalize(word string) string {
	var normalized []rune
	for _, ch := range word {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' {
			normalized = append(normalized, toLower(ch))
		} else if val, ok := equivalents[toLower(ch)]; ok {
			normalized = append(normalized, val)
		}
		// Sinon on ignore (pas de lettre équivalente)
	}
	return string(normalized)
}

// finalement, j'ai pas utilisé la distance de Levenshtein, mais je l'ai gardé pour référence, en
// en effet normaliser chaque mot avant de le comparer est certes pratique, mais la complexité est de O(n^2)
// donc j'ai développé un algorithme plus efficace de complexité O(n), en profitant de la structure de l'arbre
// préfixe, créant ainsi mon propre systeme de pénalisation, des mots équivalents, avec une incrémentation
// quadratique, pour chaque lettre équivalente, et une pénalisation de 2 pour chaque lettre
// non équivalente, et non une pénalisation linéaire, qui lorsqu'on va choisir la valeur arbitraire pour le threshhold
// de distance, on va éliminer des mots qui ne sont pas équivalents, mais qui sont proches
// et qui peuvent être pertinents, par exemple "chat" et "chats", ou "chat" et "chats", ou "chat" et "chats"
