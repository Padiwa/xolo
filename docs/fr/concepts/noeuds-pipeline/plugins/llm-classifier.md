# llm-classifier

> Plugin livré par défaut, famille « Analyse de la requête ». Retour à la [vue d'ensemble des nœuds](../index.md).

**llm-classifier** pose la question à un modèle de l'organisation, via la passerelle. Ports : `request` (requis) et `model_name` (string) en entrée. Sorties : `category`, `reason`, `error` (string), `confidence` (number).

On configure les catégories avec une phrase de description chacune, ce qui permet de trier par sujet, par service, par sensibilité, par ce qu'on veut. Le modèle interrogé vient du port `model_name` ou de la configuration. Choisissez un petit modèle. L'appel s'ajoute à la latence de chaque requête, et il n'est ni décompté du quota ni enregistré dans l'usage. En cas d'échec, `category` prend la valeur de repli et `error` explique pourquoi.

![Formulaire de configuration du plugin llm-classifier](../screenshots/inspector-classifier.png)
