import sys, json
d = json.load(sys.stdin)
vs = d.get("voices", [])
print("total voices:", len(vs))
for v in vs:
    lab = v.get("labels", {}) or {}
    keys = ("gender","accent","age","use_case","description","descriptive")
    desc = ", ".join("%s=%s" % (k, lab[k]) for k in keys if lab.get(k))
    print("- %-20s %s  [%s]  %s" % (v.get("name"), v.get("voice_id"), v.get("category"), desc))
