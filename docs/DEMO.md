# End-to-end demonstration

Flow: find a recipe → inspect the pantry → view this week's meals → propose a grocery addition → write it
with approval → read it back.

**What this run is, and is not.** The transcript below is a real run of the real `paprika-adapter` binary,
driven over stdio by a real MCP client (`cmd/demo`), on 2026-10-08. The backend was the built-in **fake
Paprika** with synthetic fixtures, served on a loopback port. It demonstrates the adapter, the MCP surface,
the approval prompt and the write/verify/retry logic. It does **not** demonstrate that Paprika's servers
accept these requests; that needs your account (see below).

```sh
make demo        # build, then: go run ./cmd/demo -adapter bin/paprika-adapter -yes
```

```text
Backend: built-in fake Paprika (synthetic fixtures), not a real account.

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
    proposal_id: FCF01ECE-104F-41C5-846C-8DF50B2611A1
    summary: Add 3 item(s) to the Paprika grocery list "My Grocery List": 1 lb chicken thighs; 2 tbsp soy sauce; 1 tbsp cornstarch.

== Step 5: Write it, with approval
  -> commit_proposal {"confirm":true,"proposal_id":"FCF01ECE-104F-41C5-846C-8DF50B2611A1"}

>>> APPROVAL REQUEST FROM THE ADAPTER
      Approve this change to Paprika?
      
      Add 3 item(s) to the Paprika grocery list "My Grocery List": 1 lb chicken thighs; 2 tbsp soy sauce; 1 tbsp cornstarch.
>>> approved automatically (-yes)
    status: committed, verified by read-back: true
    written: 1 lb chicken thighs  (uid 9120B724-B661-427C-930D-12516743D5BD)
    written: 2 tbsp soy sauce  (uid B5704651-E349-43EA-8F33-0BA8C2E773B6)
    written: 1 tbsp cornstarch  (uid 163146D1-BC94-4145-87CD-30160729BF8D)
  -> commit_proposal {"confirm":true,"proposal_id":"FCF01ECE-104F-41C5-846C-8DF50B2611A1"}
    same commit repeated: status already_committed (no second write)

== Step 6: Read it back
  -> list_grocery_items {"list_uid":"9E12FCF54A89FC52EA8E1C5DA1BDA62A6617ED8BDC2AEB6F291B93C7A399F6F6"}
    Milk
    2 lemons
    Butter
    1 lb chicken thighs
    2 tbsp soy sauce
    1 tbsp cornstarch
    sync: grocerylists is fresh (last synced 2026-10-08T16:58:48Z)
    sync: groceries is fresh (last synced 2026-10-08T16:58:48Z)

Fake Paprika saw 1 grocery write request(s) in total.
```

Things to notice:

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

This **really adds** up to `-max-items` items to the named grocery list once you type `y`. Use a disposable
list, and delete the items in the Paprika app afterwards. Until this has been run, the end-to-end flow is
demonstrated against the fake only.
