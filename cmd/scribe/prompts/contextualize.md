You are adding a retrieval-context paragraph to a raw article so that qmd's embedding index can find it from semantic queries. This follows Anthropic's Contextual Retrieval pattern: a short paragraph that situates the document within the broader KB.

Below is the raw article content. Read it and produce **one paragraph of 3–5 sentences, 60–120 words** describing:

1. **Source attribution** — "Thread by X on topic Y", "Blog post by Z about W", "Research paper on ...".
2. **3–5 named concepts, entities, decisions, or tools** from the article. Use proper nouns wherever possible — that is what embeddings match against.
3. **One-sentence framing** of what a reader would look up this article to find.

Output ONLY the paragraph — no markdown, no frontmatter, no headings, no lists, no commentary, no preamble like "Here is the context". Just the paragraph text.

## Example output

Thread by Artem Zhutov contrasting Karpathy's LLM wiki architecture with Google NotebookLM's embedding-based retrieval. Compares token cost of wiki-style ingestion (44K tokens per question across 19 sources) against NotebookLM's instant-embedding approach over 50 sources. Argues wikis are worth the cost only for PhD-level research, team wikis, or competitive analysis — and advocates converting knowledge into Claude Code skills integrated into daily routines (using Ray Dalio's 5-step decision framework as an example). Core tension: persistent structured wiki vs embedded-retrieval NotebookLM.

## Raw article

{{ARTICLE_CONTENT}}
