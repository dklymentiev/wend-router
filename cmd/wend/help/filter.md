# Filter: keep names away from the models

The filter replaces what you list with placeholders before a request leaves the
router, and puts the originals back in the answer. The model sees
`<ORG_1> signed with <PERSON_1>`; your tool gets the real names back, in streamed
answers too.

It is off until you turn on **Mask these terms in requests** at the top of the tab.

## 1. List what to hide

Type a term, pick its type, press **Add**:

| Type | For |
|---|---|
| ORG | a company: `Acme Corp` |
| PERSON | a person: `John Smith` |
| HOST | a server or domain: `db.example.lan` |
| other types | any word you like; it becomes the placeholder's name |

Terms match whole words, in any case. A dictionary of 100 to 300 terms is normal.

**Paste a list** adds many at once, one per line: a term, or a term, a comma or a
tab, and its type (`Acme Corp, ORG`).

## 2. Or hide every value of a kind

In the type list, under **Any value of a kind**, pick a kind and press **Add**. The
input is not needed: every value of that kind is masked.

| In the list | Catches |
|---|---|
| Any email | every email address |
| Any card number | card numbers that pass the check digit |
| Any bank account (IBAN) | IBANs that pass their check |
| Any account number after "account" | the number after that word |
| Any SSN | US social security numbers |
| Any phone number | numbers with separators or a leading + |
| Any passport number after "passport" | the number after that word |
| Any date of birth after "born" | the date after that word |
| Any API key or token | known key shapes, private keys, `key=...`, Bearer tokens |
| Any password | after `password:`, and inside `user:pass@host` |
| Any IP address | addresses, except 127.0.0.1 and 0.0.0.0 |
| Any URL | web addresses, except localhost and example.com |

## 3. Test it

**Test the filter** opens a panel. Paste any text and press **Test**: you see exactly
what the model would get, the masked parts highlighted. **Ask the model** sends it to
a model of your choice and shows three steps: what the model got, what it answered
(with placeholders), and what your tool gets back. **Stop** ends a slow answer.

## Your own kinds

The kinds live in `entities.json` next to the program, written from the built-in set
on first start. Edit it to change a pattern or an exception, or add a kind to its
`entities` list:

```json
{
  "id": "employee",
  "name": "Employee id",
  "type": "EMPLOYEE",
  "patterns": [{"re": "\\bEMP-[0-9]{6}\\b"}],
  "except": ["EMP-000000"]
}
```

Patterns are Go regular expressions (RE2). `group` masks only that capture group;
`check` (`luhn`, `iban`, `ssn`, `ip`) keeps only values that pass it. Restart the
router to load the changes. If the file is broken, requests that use a kind are
refused, never sent unmasked.

## What it does not do

It masks what you listed and the kinds above. It does not guess names it was not told
about: a person you did not list goes through.

## For an agent

Changes need the header `X-Router-UI: 1` and are accepted from this machine only.
The list is one string, one term a line, the term and its TYPE separated by a TAB;
a kind is `@id` instead of a term.

```
# the switch, the list, and the kinds the catalogue knows
curl -s http://127.0.0.1:8888/api/filter

# turn it on with two names and every card number
curl -s -X PUT http://127.0.0.1:8888/api/filter -H "X-Router-UI: 1" \
  -d '{"enabled": true, "terms": "Acme Corp\tORG\nJohn Smith\tPERSON\n@card\tCARD"}'

# what a model would get for a text
curl -s -X POST http://127.0.0.1:8888/api/filter/try -H "X-Router-UI: 1" \
  -d '{"text": "John Smith from Acme Corp paid with 4111 1111 1111 1111"}'
```

PUT replaces the whole list: read it first and send it back with your changes.
