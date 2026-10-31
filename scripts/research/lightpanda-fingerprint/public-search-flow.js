// Public, account-free proof that Lightpanda can drive a JS/AJAX search form.
const page = new Page();
await page.goto("https://quotes.toscrape.com/search.aspx", { waitUntil: "networkidle", timeout: 12000 });
await page.selectOption("#author", "Albert Einstein");
await page.waitForState("networkidle", { timeout: 10000 });
const tags = JSON.parse(await page.evaluate(`Array.from(document.querySelectorAll("#tag option")).map(o => ({ value: o.value, text: o.text.trim() }))`));
const inspirational = tags.find(option => /inspirational/i.test(option.text));
if (!inspirational) return { tags, error: "inspirational tag not found" };
await page.selectOption("#tag", inspirational.value);
await page.click("input[type=submit]");
await page.waitForState("networkidle", { timeout: 10000 });
const results = JSON.parse(await page.evaluate(`Array.from(document.querySelectorAll(".quote")).map(q => ({ text: q.innerText, author: q.querySelector(".author")?.innerText, tags: Array.from(q.querySelectorAll(".tag")).map(t => t.innerText) }))`));
return { url: await page.evaluate("location.href"), selectedTag: inspirational, results };
