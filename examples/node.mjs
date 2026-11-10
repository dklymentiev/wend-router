// Node 18 or newer, no packages: node node.mjs
const url = process.env.WEND_URL || "http://localhost:8888";

const res = await fetch(`${url}/v1/chat/completions`, {
  method: "POST",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify({
    model: "auto",
    messages: [{ role: "user", content: "Name three rivers in one line." }],
  }),
});
const reply = await res.json();
console.log(reply.model, "said:", reply.choices[0].message.content);
