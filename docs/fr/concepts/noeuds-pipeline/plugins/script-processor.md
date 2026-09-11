# script-processor

> Plugin livré par défaut, famille « Décision ». Retour à la [vue d'ensemble des nœuds](../index.md).

**script-processor** exécute un script [Tengo](https://github.com/d5/tengo) avec des ports libres. Le script reçoit `ctx.inputs` et `ctx.request`, renvoie des sorties et peut réécrire les messages. C'est la soupape pour tout ce que les nœuds déclaratifs ne couvrent pas. Un script qu'un `compare` et un `select` pourraient remplacer mérite d'être remplacé.
