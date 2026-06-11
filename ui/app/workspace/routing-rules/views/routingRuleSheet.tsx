/**
 * Routing Rule Dialog (Sheet)
 * Create/Edit form for routing rules
 */

import { Button } from "@/components/ui/button";
import { ComboboxSelect } from "@/components/ui/combobox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ModelMultiselect } from "@/components/ui/modelMultiselect";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Separator } from "@/components/ui/separator";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { ProviderIconType, RenderProviderIcon } from "@/lib/constants/icons";
import { getProviderLabel } from "@/lib/constants/logs";
import { getErrorMessage } from "@/lib/store";
import { useGetCustomersQuery, useGetTeamsQuery, useGetVirtualKeysQuery } from "@/lib/store/apis/governanceApi";
import { useGetAllKeysQuery, useGetProvidersQuery } from "@/lib/store/apis/providersApi";
import { useCreateRoutingRuleMutation, useGetRoutingRulesQuery, useUpdateRoutingRuleMutation } from "@/lib/store/apis/routingRulesApi";
import { DEFAULT_ROUTING_RULE_FORM_DATA, ROUTING_RULE_SCOPES, RoutingRule, RoutingRuleFormData } from "@/lib/types/routingRules";
import { validateRateLimitAndBudgetRules, validateRoutingRules } from "@/lib/utils/celConverterRouting";
import { normalizeRoutingRuleGroupQuery } from "@/lib/utils/routingRuleGroupQuery";
import { RbacOperation, RbacResource, useRbac } from "@enterprise/lib";
import { Plus, Trash2, X } from "lucide-react";
import { lazy, Suspense, useCallback, useEffect, useState } from "react";
import { useForm } from "react-hook-form";
import { RuleGroupType } from "react-querybuilder";
import { toast } from "sonner";

interface RoutingRuleDialogProps {
	open: boolean;
	onOpenChange: (open: boolean) => void;
	editingRule?: RoutingRule | null;
	onSuccess?: () => void;
}

const defaultQuery: RuleGroupType = {
	combinator: "and",
	rules: [],
};

// Lazy-load CEL builder (heavy dependency tree).
const CELRuleBuilderLazy = lazy(() =>
	import("@/app/workspace/routing-rules/components/celBuilder/celRuleBuilder").then((mod) => ({
		default: mod.CELRuleBuilder,
	})),
);
const CELRuleBuilder = (props: React.ComponentProps<typeof CELRuleBuilderLazy>) => (
	<Suspense fallback={<div className="text-sm text-gray-500">Loading CEL builder...</div>}>
		<CELRuleBuilderLazy {...props} />
	</Suspense>
);

export function RoutingRuleSheet({ open, onOpenChange, editingRule, onSuccess }: RoutingRuleDialogProps) {
	const { data: rulesData } = useGetRoutingRulesQuery();
	const rules = rulesData?.rules || [];
	const { data: providersData = [] } = useGetProvidersQuery();
	const { data: allKeysData = [] } = useGetAllKeysQuery();
	const { data: vksData = { virtual_keys: [] } } = useGetVirtualKeysQuery();
	const { data: teamsData = { teams: [], count: 0, total_count: 0, limit: 0, offset: 0 } } = useGetTeamsQuery();
	const { data: customersData = { customers: [] } } = useGetCustomersQuery();
	const [createRoutingRule, { isLoading: isCreating }] = useCreateRoutingRuleMutation();
	const [updateRoutingRule, { isLoading: isUpdating }] = useUpdateRoutingRuleMutation();

	const [query, setQuery] = useState<RuleGroupType>(defaultQuery);
	const [builderKey, setBuilderKey] = useState(0);

	const {
		register,
		handleSubmit,
		setValue,
		watch,
		reset,
		formState: { errors },
	} = useForm<RoutingRuleFormData>({
		defaultValues: DEFAULT_ROUTING_RULE_FORM_DATA,
	});

	const isEditing = !!editingRule;
	const isLoading = isCreating || isUpdating;
	const canCreate = useRbac(RbacResource.RoutingRules, RbacOperation.Create);
	const canUpdate = useRbac(RbacResource.RoutingRules, RbacOperation.Update);
	const hasRequiredAccess = isEditing ? canUpdate : canCreate;
	const enabled = watch("enabled");
	const chainRule = watch("chain_rule");
	const scope = watch("scope");
	const scopeId = watch("scope_id");
	const fallbacks = watch("fallbacks");
	const provider = watch("provider");
	const model = watch("model");
	const keyId = watch("key_id");

	const availableProviders = Array.from(
		new Set([
			...providersData.map((p) => p.name),
			...(provider ? [provider] : []),
			...(rules.map((r) => r.provider).filter(Boolean) as string[]),
			...rules.flatMap((r) => (r.fallbacks ?? []).map((f) => f.split("/")[0]?.trim()).filter(Boolean)),
		]),
	);

	// Initialize form data when editing rule changes
	useEffect(() => {
		if (editingRule) {
			setValue("id", editingRule.id);
			setValue("name", editingRule.name);
			setValue("description", editingRule.description);
			setValue("cel_expression", editingRule.cel_expression);
			setValue("fallbacks", editingRule.fallbacks || []);
			setValue("scope", editingRule.scope);
			setValue("scope_id", editingRule.scope_id || "");
			setValue("priority", editingRule.priority);
			setValue("enabled", editingRule.enabled);
			setValue("chain_rule", editingRule.chain_rule ?? false);
			setValue("provider", editingRule.provider || "");
			setValue("model", editingRule.model || "");
			setValue("key_id", editingRule.key_id || "");
			// Only react-querybuilder-shaped queries are valid; config may store other JSON under `query`.
			setQuery(normalizeRoutingRuleGroupQuery(editingRule.query));
			setBuilderKey((prev) => prev + 1);
		} else {
			reset();
			setQuery(defaultQuery);
			setBuilderKey((prev) => prev + 1);
		}
	}, [editingRule, open, setValue, reset]);

	const handleQueryChange = useCallback(
		(expression: string, newQuery: RuleGroupType) => {
			setValue("cel_expression", expression);
			setQuery(newQuery);
		},
		[setValue],
	);

	const onSubmit = (data: RoutingRuleFormData) => {
		// Validate scope_id is required when scope is not global
		if (data.scope !== "global" && !data.scope_id?.trim()) {
			toast.error(`${data.scope === "team" ? "Team" : data.scope === "customer" ? "Customer" : "Virtual Key"} is required`);
			return;
		}

		if (data.key_id?.trim() && !data.provider?.trim()) {
			toast.error("Provider is required when pinning a key");
			return;
		}

		// Validate regex patterns in routing rules
		const regexErrors = validateRoutingRules(query);
		if (regexErrors.length > 0) {
			toast.error(`Invalid regex pattern:\n${regexErrors.join("\n")}`);
			return;
		}

		// Validate rate limit and budget rules
		const rateLimitErrors = validateRateLimitAndBudgetRules(query);
		if (rateLimitErrors.length > 0) {
			toast.error(`Invalid rule configuration:\n${rateLimitErrors.join("\n")}`);
			return;
		}

		// Filter out incomplete fallbacks (empty provider)
		const validFallbacks = (data.fallbacks || []).filter((fb) => {
			const provider = fb.split("/")[0]?.trim();
			return provider && provider.length > 0;
		});

		const payload = {
			name: data.name,
			description: data.description,
			cel_expression: data.cel_expression,
			provider: data.provider?.trim() || undefined,
			model: data.model?.trim() || undefined,
			key_id: data.key_id?.trim() || undefined,
			fallbacks: validFallbacks,
			scope: data.scope,
			scope_id: data.scope === "global" ? undefined : data.scope_id || undefined,
			priority: data.priority,
			enabled: data.enabled,
			chain_rule: data.chain_rule,
			query: query,
		};

		const submitPromise =
			isEditing && editingRule
				? updateRoutingRule({
						id: editingRule.id,
						data: payload,
					}).unwrap()
				: createRoutingRule(payload).unwrap();

		submitPromise
			.then(() => {
				toast.success(isEditing ? "Routing rule updated successfully" : "Routing rule created successfully");
				reset();
				setQuery(defaultQuery);
				setBuilderKey((prev) => prev + 1);
				onOpenChange(false);
				onSuccess?.();
			})
			.catch((error: any) => {
				toast.error(getErrorMessage(error));
			});
	};

	const handleCancel = () => {
		reset();
		setQuery(defaultQuery);
		setBuilderKey((prev) => prev + 1);
		onOpenChange(false);
	};

	return (
		<Sheet open={open} onOpenChange={onOpenChange}>
			<SheetContent className="flex w-full min-w-1/2 flex-col gap-4 overflow-x-hidden p-0 pt-4">
				<SheetHeader className="flex flex-col items-start px-8 py-4" headerClassName="mb-0 sticky -top-4 bg-card z-10">
					<SheetTitle>{isEditing ? "Edit Routing Rule" : "Create New Routing Rule"}</SheetTitle>
					<SheetDescription>
						{isEditing ? "Update the routing rule configuration" : "Create a new CEL-based routing rule for intelligent request routing"}
					</SheetDescription>
				</SheetHeader>

				<form onSubmit={handleSubmit(onSubmit)} className="flex grow flex-col">
					<div className="flex grow flex-col gap-6 px-8 pb-6">
						{/* Rule Name */}
						<div className="space-y-3">
							<Label htmlFor="name">
								Rule Name <span className="text-red-500">*</span>
							</Label>
							<Input
								id="name"
								placeholder="e.g., Route GPT-4 to Azure"
								{...register("name", { required: "Rule name is required", maxLength: 255 })}
							/>
							{errors.name && <p className="text-destructive text-sm">{errors.name.message}</p>}
						</div>

						{/* Description */}
						<div className="space-y-3">
							<Label htmlFor="description">Description</Label>
							<Textarea id="description" placeholder="Describe what this rule does..." rows={2} {...register("description")} />
						</div>

						{/* Enabled Switch */}
						<div className="flex items-center justify-between rounded-lg border p-4">
							<div className="space-y-0.5">
								<Label htmlFor="enabled">Enable Rule</Label>
								<p className="text-muted-foreground text-sm">Rule will be active and applied to matching requests</p>
							</div>
							<Switch id="enabled" checked={enabled} onCheckedChange={(checked) => setValue("enabled", checked)} />
						</div>

						{/* Chain Rule Switch */}
						<div className="flex items-center justify-between rounded-lg border p-4">
							<div className="space-y-0.5">
								<Label htmlFor="chain_rule">Chain Rule</Label>
								<p className="text-muted-foreground text-sm">
									After this rule matches, re-evaluate routing rules using the resolved provider/model as the new context. Useful for
									composing rules — e.g. normalize a model alias first, then route based on the canonical name.
								</p>
							</div>
							<Switch
								id="chain_rule"
								checked={chainRule}
								onCheckedChange={(checked) => setValue("chain_rule", checked)}
								data-testid="routing-rule-chain-rule-switch"
							/>
						</div>

						{/* Scope and Priority - Side by Side */}
						<div className="grid grid-cols-2 gap-4">
							<div className="space-y-3">
								<Label htmlFor="scope">Scope</Label>
								<Select
									value={scope}
									onValueChange={(value) => {
										setValue("scope", value as any);
										// Clear scope_id when scope changes
										setValue("scope_id", "");
									}}
								>
									<SelectTrigger className="w-full">
										<SelectValue placeholder="Select scope..." />
									</SelectTrigger>
									<SelectContent>
										{ROUTING_RULE_SCOPES.map((scopeOption) => (
											<SelectItem key={scopeOption.value} value={scopeOption.value}>
												{scopeOption.label}
											</SelectItem>
										))}
									</SelectContent>
								</Select>
							</div>

							<div className="space-y-3">
								<Label htmlFor="priority">
									Priority <span className="text-red-500">*</span>
								</Label>
								<Input
									id="priority"
									type="number"
									min={0}
									max={1000}
									{...register("priority", {
										required: "Priority is required",
										min: { value: 0, message: "Priority must be ≥ 0" },
										max: { value: 1000, message: "Priority must be ≤ 1000" },
										valueAsNumber: true,
									})}
								/>
								<p className="text-muted-foreground text-xs">Lower numbers = higher priority (0 is highest)</p>
								{errors.priority && <p className="text-destructive text-sm">{errors.priority.message}</p>}
							</div>
						</div>

						{scope !== "global" && (
							<div className="space-y-2">
								<Label htmlFor="scope_id">
									{scope === "team" ? "Team" : scope === "customer" ? "Customer" : "Virtual Key"} <span className="text-red-500">*</span>
								</Label>
								{scope === "team" && teamsData.teams.length > 0 && (
									<ComboboxSelect
										options={teamsData.teams.map((team) => ({ label: team.name, value: team.id }))}
										value={scopeId || null}
										onValueChange={(value) => setValue("scope_id", value ?? "")}
										placeholder="Select a team..."
										noPortal
									/>
								)}
								{scope === "customer" && customersData.customers.length > 0 && (
									<ComboboxSelect
										options={customersData.customers.map((customer) => ({ label: customer.name, value: customer.id }))}
										value={scopeId || null}
										onValueChange={(value) => setValue("scope_id", value ?? "")}
										placeholder="Select a customer..."
										noPortal
									/>
								)}
								{scope === "virtual_key" && vksData.virtual_keys.length > 0 && (
									<ComboboxSelect
										options={vksData.virtual_keys.map((vk) => ({ label: vk.name, value: vk.id }))}
										value={scopeId || null}
										onValueChange={(value) => setValue("scope_id", value ?? "")}
										placeholder="Select a virtual key..."
										noPortal
									/>
								)}
								{((scope === "team" && teamsData.teams.length === 0) ||
									(scope === "customer" && customersData.customers.length === 0) ||
									(scope === "virtual_key" && vksData.virtual_keys.length === 0)) && (
									<p className="text-muted-foreground text-sm">
										No {scope === "team" ? "teams" : scope === "customer" ? "customers" : "virtual keys"} available
									</p>
								)}
								{errors.scope_id && <p className="text-destructive text-sm">{errors.scope_id.message}</p>}
							</div>
						)}

						<Separator />

						{/* CEL Rule Builder */}
						<div className="space-y-3">
							<Label>Rule Builder</Label>
							<p className="text-muted-foreground text-sm">
								Build conditions to determine when this rule should apply. Leave empty to apply this rule to all requests.
							</p>
							<CELRuleBuilder
								key={builderKey}
								initialQuery={query}
								onChange={handleQueryChange}
								providers={availableProviders}
								models={[]}
								allowCustomModels={true}
							/>
						</div>

						{/* Note about Token/Request Limits and Budget Configuration */}
						<p className="text-muted-foreground text-xs">
							Note: Ensure token limits, request limits, and budget are configured in{" "}
							<strong>Model Providers → Configurations → {"{provider}"} → Governance</strong> (provider-level) or{" "}
							<strong>Model Providers → Budgets & Limits</strong> section (model-level) before using them in routing rules.
						</p>

						<Separator />

						{/* Routing Output */}
						<div className="space-y-3">
							<div>
								<Label>Routing Output</Label>
								<p className="text-muted-foreground mt-0.5 text-xs">
									Leave provider or model empty to use the incoming request value.
								</p>
							</div>
							<RoutingOutputFields
								provider={provider}
								model={model}
								keyId={keyId}
								availableProviders={availableProviders}
								allKeys={allKeysData}
								onProviderChange={(value) => {
									setValue("provider", value);
									setValue("model", "");
									setValue("key_id", "");
								}}
								onModelChange={(value) => setValue("model", value)}
								onKeyIdChange={(value) => setValue("key_id", value)}
							/>
						</div>

						{/* Fallbacks */}
						<div className="space-y-3">
							<div className="flex items-center justify-between">
								<div>
									<Label>Fallbacks</Label>{" "}
									<p className="text-muted-foreground mt-0.5 text-xs">
										Provider is required, but model is optional. Leave model empty to use the incoming request value.
									</p>
								</div>
								<Button
									type="button"
									variant="outline"
									size="sm"
									onClick={() => setValue("fallbacks", [...(fallbacks || []), ""])}
									className="gap-2"
								>
									<Plus className="h-4 w-4" />
									Add Fallback
								</Button>
							</div>
							<div className="space-y-2">
								{(fallbacks || []).length === 0 ? (
									<p className="text-muted-foreground text-sm">No fallbacks configured</p>
								) : (
									(fallbacks || []).map((fallback, index) => {
										// Parse provider/model from fallback string
										const parts = fallback.split("/");
										const fbProvider = parts[0] || "";
										const fbModel = parts[1] || "";

										const handleProviderChange = (newProvider: string) => {
											const model = fbModel || "";
											const newFallback = `${newProvider}/${model}`;
											const newFallbacks = [...fallbacks];
											newFallbacks[index] = newFallback;
											setValue("fallbacks", newFallbacks);
										};

										const handleModelChange = (newModel: string) => {
											const prov = fbProvider || "";
											const newFallback = `${prov}/${newModel}`;
											const newFallbacks = [...fallbacks];
											newFallbacks[index] = newFallback;
											setValue("fallbacks", newFallbacks);
										};

										const handleRemove = () => {
											const newFallbacks = fallbacks.filter((_: string, i: number) => i !== index);
											setValue("fallbacks", newFallbacks);
										};

										return (
											<div key={index} className="flex items-center gap-2">
												<div className="flex-1">
													<Select value={fbProvider} onValueChange={handleProviderChange}>
														<SelectTrigger className="w-full">
															<SelectValue placeholder="Select provider..." />
														</SelectTrigger>
														<SelectContent>
															{availableProviders.map((prov) => (
																<SelectItem key={prov} value={prov}>
																	<div className="flex items-center gap-2">
																		<RenderProviderIcon provider={prov as ProviderIconType} size="sm" className="h-4 w-4" />
																		<span>{getProviderLabel(prov)}</span>
																	</div>
																</SelectItem>
															))}
														</SelectContent>
													</Select>
												</div>
												<div className="flex-1">
													<ModelMultiselect
														provider={fbProvider || undefined}
														value={fbModel}
														onChange={handleModelChange}
														placeholder="Incoming (optional)"
														isSingleSelect
														disabled={!fbProvider}
														className="!h-9 !min-h-9 w-full"
													/>
												</div>
												<Button
													type="button"
													variant="ghost"
													size="sm"
													onClick={handleRemove}
													className="h-9 px-2"
													aria-label={`Remove fallback ${index + 1}`}
												>
													<Trash2 className="h-4 w-4" />
												</Button>
											</div>
										);
									})
								)}
							</div>
							<p className="text-muted-foreground text-xs">Fallbacks will be used in the order they are defined</p>
						</div>
					</div>
					{/* Action Buttons */}
					<div className="bg-card sticky bottom-0 flex justify-end gap-3 border-t px-8 py-4">
						<Button type="button" variant="outline" onClick={handleCancel} disabled={isLoading}>
							Cancel
						</Button>
						<Button type="submit" disabled={isLoading || !hasRequiredAccess}>
							{isEditing ? "Update Rule" : "Save Rule"}
						</Button>
					</div>
				</form>
			</SheetContent>
		</Sheet>
	);
}

interface RoutingOutputFieldsProps {
	provider: string;
	model: string;
	keyId: string;
	availableProviders: string[];
	allKeys: Array<{ key_id: string; name: string; provider: string }>;
	onProviderChange: (value: string) => void;
	onModelChange: (value: string) => void;
	onKeyIdChange: (value: string) => void;
}

function RoutingOutputFields({
	provider,
	model,
	keyId,
	availableProviders,
	allKeys,
	onProviderChange,
	onModelChange,
	onKeyIdChange,
}: RoutingOutputFieldsProps) {
	const availableKeys = provider ? allKeys.filter((k) => k.provider === provider).map((k) => ({ id: k.key_id, name: k.name })) : [];

	return (
		<div className="space-y-3 rounded-lg border p-3" data-testid="routing-output-fields">
			<div className="grid grid-cols-2 gap-3">
				<div className="space-y-1.5">
					<Label id="routing-output-provider-label" className="text-xs">
						Provider
					</Label>
					<div className="flex gap-1.5">
						<Select value={provider} onValueChange={onProviderChange}>
							<SelectTrigger
								id="routing-output-provider-select"
								aria-labelledby="routing-output-provider-label"
								className="h-9 flex-1 text-sm"
								data-testid="routing-output-provider-select"
							>
								<SelectValue placeholder="Incoming (optional)" />
							</SelectTrigger>
							<SelectContent>
								{availableProviders.map((prov) => (
									<SelectItem key={prov} value={prov}>
										<div className="flex items-center gap-2">
											<RenderProviderIcon provider={prov as ProviderIconType} size="sm" className="h-4 w-4" />
											<span>{getProviderLabel(prov)}</span>
										</div>
									</SelectItem>
								))}
							</SelectContent>
						</Select>
						{provider && (
							<Button
								type="button"
								variant="outline"
								size="sm"
								onClick={() => onProviderChange("")}
								className="h-9 w-9 p-0"
								aria-label="Clear provider"
								data-testid="routing-output-provider-clear"
							>
								<X className="h-3.5 w-3.5" />
							</Button>
						)}
					</div>
				</div>

				<div className="space-y-1.5">
					<Label id="routing-output-model-label" className="text-xs">
						Model
					</Label>
					<div className="flex gap-1.5">
						<div className="flex-1" data-testid="routing-output-model-select">
							<ModelMultiselect
								provider={provider || undefined}
								value={model}
								onChange={onModelChange}
								placeholder="Incoming (optional)"
								isSingleSelect
								loadModelsOnEmptyProvider
								className="!h-9 !min-h-9"
								inputId="routing-output-model-input"
								ariaLabelledBy="routing-output-model-label"
							/>
						</div>
						{model && (
							<Button
								type="button"
								variant="outline"
								size="sm"
								onClick={() => onModelChange("")}
								className="h-9 w-9 p-0"
								aria-label="Clear model"
								data-testid="routing-output-model-clear"
							>
								<X className="h-3.5 w-3.5" />
							</Button>
						)}
					</div>
				</div>
			</div>

			{provider && (availableKeys.length > 0 || keyId) && (
				<div className="space-y-1.5">
					<Label id="routing-output-apikey-label" className="text-xs">
						API Key <span className="text-muted-foreground">(optional — leave unset for load-balanced selection)</span>
					</Label>
					<div className="flex gap-1.5">
						<Select value={keyId || ""} onValueChange={onKeyIdChange}>
							<SelectTrigger
								id="routing-output-apikey-select"
								aria-labelledby="routing-output-apikey-label"
								className="h-9 flex-1 text-sm"
								data-testid="routing-output-apikey-select"
							>
								<SelectValue placeholder="Select key (optional)" />
							</SelectTrigger>
							<SelectContent>
								{availableKeys.map((key) => (
									<SelectItem key={key.id} value={key.id}>
										{key.name}
									</SelectItem>
								))}
								{keyId && !availableKeys.some((k) => k.id === keyId) && (
									<SelectItem key={`pinned-${keyId}`} value={keyId}>
										(pinned) {keyId}
									</SelectItem>
								)}
							</SelectContent>
						</Select>
						{keyId && (
							<Button
								type="button"
								variant="outline"
								size="sm"
								onClick={() => onKeyIdChange("")}
								className="h-9 w-9 p-0"
								aria-label="Clear API key"
								data-testid="routing-output-apikey-clear"
							>
								<X className="h-3.5 w-3.5" />
							</Button>
						)}
					</div>
				</div>
			)}
		</div>
	);
}