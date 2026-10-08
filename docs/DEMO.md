# End-to-end demonstration

Flow: connect with a token → find a recipe → inspect the pantry → view this week's meals → propose a grocery addition → write it
with approval → read it back.

**What this run is, and is not.** The transcript below is a real run of the real `paprika-adapter` binary,
started as a separate process and driven over HTTP by a real MCP client (`cmd/demo`), on 2026-10-08. The demo
makes a token with `paprika-adapter token` and starts the server with it on a free loopback port. The backend
was the built-in **fake Paprika** with synthetic fixtures, served on another loopback port. It demonstrates
the adapter, token authentication, the MCP surface, the approval prompt and the write/verify/retry logic.
It does **not** demonstrate that Paprika's servers accept these requests; that needs your account (see below).

```sh
make demo        # build, then: go run ./cmd/demo -adapter bin/paprika-adapter -yes
```

```text
Backend: built-in fake Paprika (synthetic fixtures), not a real account.

== Step 0: Connect to http://127.0.0.1:62860/mcp
    no token     -> HTTP 401
time=2026-10-08T21:47:15.511+02:00 level=WARN msg="mcp request with an unknown token rejected" remote=127.0.0.1:62868
    wrong token  -> HTTP 401
    demo's token -> connected

== Step 1: Find a recipe
  -> search_recipes {"limit":3,"query":"chicken"}
    Weeknight Chicken Stir-Fry  (uid 3B9E1D42-6A7C-4F85-9D20-1C8E5A4B7F63)
    Lemon Garlic Roast Chicken  (uid 014031E3-BC74-4CA8-B345-36EBE9C9794B)
  -> get_recipe {"uid":"3B9E1D42-6A7C-4F85-9D20-1C8E5A4B7F63"}
    ingredients_raw of "Weeknight Chicken Stir-Fry":
      1 lb chicken thighs, sliced
      2 tbsp soy sauce
      1 tbsp cornstarch
      1 red bell pepper, sliced
      2 cups broccoli florets
      3 cloves garlic, minced
      1 tbsp grated ginger
      2 cups cooked rice, to serve

== Step 2: Inspect the pantry
  -> list_pantry {"in_stock_only":true}
    in stock: bread flour, garlic, ground cumin, olive oil, red lentils, rice, vegetable stock

== Step 3: View this week's meals
  -> get_meal_plan null
    week 2026-10-05 to 2026-10-11
    2026-10-05  Dinner    Red Lentil Soup
    2026-10-06  Lunch     Leftover soup
    2026-10-07  Dinner    Weeknight Chicken Stir-Fry
    2026-10-10  Breakfast Buttermilk Pancakes

== Step 4: Propose a grocery addition (nothing is written yet)
  -> list_grocery_lists null
  -> propose_grocery_items {"items":[{"name":"chicken thighs","quantity":"1 lb"},{"name":"soy sauce","quantity":"2 tbsp"},{"name":"cornstarch","quantity":"1 tbsp"}],"list_uid":"9E12FCF54A89FC52EA8E1C5DA1BDA62A6617ED8BDC2AEB6F291B93C7A399F6F6"}
    proposal_id: 6CA068AF-3806-4393-99C8-B744C4848D33
    summary: Add 3 item(s) to the Paprika grocery list "My Grocery List": 1 lb chicken thighs; 2 tbsp soy sauce; 1 tbsp cornstarch.

== Step 5: Write it, with approval
  -> commit_proposal {"confirm":true,"proposal_id":"6CA068AF-3806-4393-99C8-B744C4848D33"}

>>> APPROVAL REQUEST FROM THE ADAPTER
      Approve this change to Paprika?
      
      Add 3 item(s) to the Paprika grocery list "My Grocery List": 1 lb chicken thighs; 2 tbsp soy sauce; 1 tbsp cornstarch.
>>> approved automatically (-yes)
    status: committed, verified by read-back: true
    written: 1 lb chicken thighs  (uid 9F1CC058-7A36-4D0B-90D1-19C12C536323)
    written: 2 tbsp soy sauce  (uid B0A67E63-E475-4AE3-AABA-E7C75E8C679B)
    written: 1 tbsp cornstarch  (uid 0A4431BA-65A9-483A-8D62-03868AA96DDA)
  -> commit_proposal {"confirm":true,"proposal_id":"6CA068AF-3806-4393-99C8-B744C4848D33"}
    same commit repeated: status already_committed (no second write)

== Step 6: Read it back
  -> list_grocery_items {"list_uid":"9E12FCF54A89FC52EA8E1C5DA1BDA62A6617ED8BDC2AEB6F291B93C7A399F6F6"}
    Milk
    2 lemons
    Butter
    1 lb chicken thighs
    2 tbsp soy sauce
    1 tbsp cornstarch
    sync: grocerylists is fresh (last synced 2026-10-08T19:47:15Z)
    sync: groceries is fresh (last synced 2026-10-08T19:47:15Z)

Fake Paprika saw 1 grocery write request(s) in total.
```

Things to notice:

- Step 0: requests without a token, or with an unknown one, get HTTP 401 before reaching the MCP server.
  The `WARN` line is the adapter's own log; it names the remote address, never the token.
- Step 4 wrote nothing: the proposal exists only in the adapter's SQLite file.
- Step 5 shows the approval request the adapter sent to the client through MCP elicitation. How it
  is shown is up to the client: a prompt in a CLI, buttons in a chat. Declining ends the commit with `approval_declined`.
- The commit result lists the items **as read back from the backend**, not as proposed.
- Repeating the commit returned `already_committed`; the backend saw exactly one write request.
- Step 6 reads through the normal read tool, with the freshness of the data it used.

`-raw` prints every tool result as JSON. `-query`, `-list` and `-max-items` change what is searched and proposed.

## Running it against your account

Same flow, real Paprika, and you answer the approval prompt yourself:

```sh
make build
set -a; . ~/.config/paprika-adapter/paprika.env; set +a
go run ./cmd/demo -adapter bin/paprika-adapter -live -query lentil -list "adapter-test" -max-items 1
```

The demo starts its own adapter process on a free port with a one-off token, so it does not disturb a
running service. This **really adds** up to `-max-items` items to the named grocery list once you type `y`. Use a disposable
list, and delete the items in the Paprika app afterwards. Until this has been run, the end-to-end flow is
demonstrated against the fake only.
