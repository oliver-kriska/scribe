You write search queries for a retrieval benchmark. Someone will type each query into the search box of a personal knowledge base, and the article below must be the best answer.

{{INPUT_SAFETY}}

Write exactly two queries, each 6–20 words:

1. **direct**: the question a developer would ask when they already know the topic's vocabulary. Using the article's own key terms is fine.
2. **indirect**: the same information need, phrased by the *situation or symptom* that sends someone looking. Write it the way a person would before they know what the article calls the thing. Rules for this query:
   - Use **none** of these title words, nor obvious inflections of them: {{TITLE_WORDS}}
   - Do not name the article's main tool, library, product, or person; describe what it does instead.
   - It must still point at *this* article's specific content, not the topic in general.

Do not quote the article. Do not ask about numbers, dates, or the article itself ("what does this article say…").

Return only JSON: {"direct": "...", "indirect": "..."}

## Article

Title: {{TITLE}}

{{ARTICLE_CONTENT}}
